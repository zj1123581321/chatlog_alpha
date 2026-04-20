package wechat

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog/log"

	"github.com/sjzar/chatlog/internal/errors"
	"github.com/sjzar/chatlog/internal/wechat"
	"github.com/sjzar/chatlog/internal/wechat/decrypt"
	"github.com/sjzar/chatlog/internal/wechat/decrypt/common"
	"github.com/sjzar/chatlog/pkg/filemonitor"
	"github.com/sjzar/chatlog/pkg/util"
)

var (
	// DebounceTime 是微信需要连续安静多久 chatlog 才开始解密。
	// 默认 60 秒，让打开图片/收发消息等日常活动完全不会触发 chatlog 抢 IO。
	// 用户可通过 AutoDecryptDebounce 配置覆盖。
	DebounceTime = 60 * time.Second

	// MaxWaitTime 是即使微信从未真正安静，也至少隔多久强制跑一次的兜底。
	// 默认 10 分钟：即使用户持续活跃，chatlog 每 10 分钟至少追平一次。
	MaxWaitTime = 10 * time.Minute
)

type Service struct {
	conf           Config
	lastEvents     map[string]time.Time
	pendingActions map[string]bool
	pendingEvents  map[string]*pendingEvent
	walStates      map[string]*walState
	mutex          sync.Mutex
	fm             *filemonitor.FileMonitor
	errorHandler   func(error)
	decryptSem     chan struct{}

	// decryptCtx 是所有 autodecrypt 后台 goroutine（waitAndProcess 以及 Stage G
	// 的 firstFullDecrypt）共享的取消上下文。StopAutoDecrypt 时 cancel，让长
	// backoff sleep 和未来的 blocking op 能及时退出。每次 StartAutoDecrypt
	// 刷新一次（旧 ctx 已被上次 Stop 取消）。
	decryptCtx    context.Context
	decryptCancel context.CancelFunc

	// decryptWg 追踪所有在跑的 autodecrypt goroutine。Stop 时 cancel + Wait(5s)
	// 保证切账号 / 退出 TUI 时不会泄漏 goroutine 到新上下文。
	decryptWg sync.WaitGroup

	// phaseState 显式追踪自动解密生命周期（Idle/Precheck/FirstFull/Live/Failed/Stopping）
	// + 上次运行摘要。消费者：TUI 状态栏 / HTTP /status / HTTP 503 gate。
	phaseState phaseState
}

// stopTimeout 是 StopAutoDecrypt 等待后台 goroutine 清理的最长时间。
// 超时后打 warn 日志但继续返回 —— 不阻塞切账号 / 退出动作。
// 用 var 而非 const 方便单测注入更短值。
var stopTimeout = 5 * time.Second

type pendingEvent struct {
	sawDB  bool
	sawWal bool
}

type walState struct {
	offset int64
	salt1  uint32
	salt2  uint32
}

type walFrame struct {
	pageNo uint32
	data   []byte
}

type Config interface {
	GetDataKey() string
	GetDataDir() string
	GetWorkDir() string
	GetPlatform() string
	GetVersion() int
	GetWalEnabled() bool
	GetAutoDecryptDebounce() int
}

func NewService(conf Config) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		conf:           conf,
		lastEvents:     make(map[string]time.Time),
		pendingActions: make(map[string]bool),
		pendingEvents:  make(map[string]*pendingEvent),
		walStates:      make(map[string]*walState),
		decryptSem:     make(chan struct{}, 1),
		decryptCtx:     ctx,
		decryptCancel:  cancel,
		phaseState:     newPhaseState(),
	}
}

// acquireDecryptSlot blocks until a decrypt slot is available.
// Limits concurrent decryption to 1 to minimize IO contention with WeChat.
func (s *Service) acquireDecryptSlot() {
	s.decryptSem <- struct{}{}
}

func (s *Service) releaseDecryptSlot() {
	<-s.decryptSem
}

// handleDecryptError routes errors: file lock errors are logged and skipped,
// other errors trigger the circuit-breaker errorHandler.
func (s *Service) handleDecryptError(err error) {
	if err == nil {
		return
	}
	if util.IsFileLockError(err) {
		log.Warn().Err(err).Msg("文件被微信占用，跳过本次解密")
		return
	}
	if s.errorHandler != nil {
		s.errorHandler(err)
	}
}

// recoverDecryptPanic 捕获 autodecrypt goroutine 的 panic，记录日志 + 堆栈，
// 并触发熔断 handler 让 TUI 弹错 + SetAutoDecrypt(false)。
//
// 项目约定：所有 autodecrypt 相关的后台 goroutine 必须在第一条 defer 使用：
//
//	defer s.recoverDecryptPanic("goroutine-name")
//
// 避免单个 goroutine panic 炸掉整个 chatlog 进程（历史上曾有过 context 死锁
// 和 handle 泄漏问题，长期运行稳定性容忍度低）。
func (s *Service) recoverDecryptPanic(op string) {
	if r := recover(); r != nil {
		err := fmt.Errorf("autodecrypt %s goroutine panic: %v", op, r)
		log.Error().
			Str("op", op).
			Interface("panic", r).
			Bytes("stack", debug.Stack()).
			Msg("[autodecrypt] goroutine panic recovered")
		if s.errorHandler != nil {
			s.errorHandler(err)
		}
	}
}

// SetAutoDecryptErrorHandler sets the callback for auto decryption errors
func (s *Service) SetAutoDecryptErrorHandler(handler func(error)) {
	s.errorHandler = handler
}

// GetWeChatInstances returns all running WeChat instances
func (s *Service) GetWeChatInstances() []*wechat.Account {
	instances, _ := s.GetWeChatInstancesWithError()
	return instances
}

func (s *Service) GetWeChatInstancesWithError() ([]*wechat.Account, error) {
	if err := wechat.Load(); err != nil {
		return nil, err
	}
	return wechat.GetAccounts(), nil
}

// GetDataKey extracts the encryption key from a WeChat process
func (s *Service) GetDataKey(info *wechat.Account) (string, error) {
	if info == nil {
		return "", fmt.Errorf("no WeChat instance selected")
	}

	key, _, err := info.GetKey(context.Background())
	if err != nil {
		return "", err
	}

	return key, nil
}

// GetImageKey extracts the image key from a WeChat process
func (s *Service) GetImageKey(info *wechat.Account) (string, error) {
	if info == nil {
		return "", fmt.Errorf("no WeChat instance selected")
	}

	return info.GetImageKey(context.Background())
}

func (s *Service) StartAutoDecrypt() error {
	// 如果上次 Stop 已 cancel 了 ctx，重建一份供本轮 goroutine 使用。
	s.mutex.Lock()
	if s.decryptCtx == nil || s.decryptCtx.Err() != nil {
		s.decryptCtx, s.decryptCancel = context.WithCancel(context.Background())
	}
	s.mutex.Unlock()

	log.Info().
		Str("data_dir", s.conf.GetDataDir()).
		Dur("quiet_period", s.getDebounceTime()).
		Dur("max_wait", s.getMaxWaitTime()).
		Msg("自动解密已启用：微信安静期达到后处理变更，最长兜底强制处理")
	// Always monitor WAL files since WeChat uses WAL mode regardless of our setting.
	// When WalEnabled is false, WAL changes still trigger a full re-decrypt of the main .db file.
	pattern := `.*\.db(-wal|-shm)?$`
	// rootDir 窄化到 db_storage 子目录：data dir 下 msg/attach/ 有 9.6 万+ 图片文件，
	// filemonitor 初始化会 fs.WalkDir 整个 rootDir 找匹配 .db 的目录，实测整个 data dir
	// 要 17 秒；窄化到 db_storage (仅几十个 .db 文件) <2 秒。所有微信 db 都在这里。
	dbStorage := filepath.Join(s.conf.GetDataDir(), "db_storage")
	dbGroup, err := filemonitor.NewFileGroup("wechat", dbStorage, pattern, []string{"fts"})
	if err != nil {
		return err
	}
	dbGroup.AddCallback(s.DecryptFileCallback)

	s.fm = filemonitor.NewFileMonitor()
	s.fm.AddGroup(dbGroup)
	if err := s.fm.Start(); err != nil {
		log.Debug().Err(err).Msg("failed to start file monitor")
		return err
	}
	return nil
}

func (s *Service) StopAutoDecrypt() error {
	// 1. 停文件监听 —— 不再 spawn 新的 waitAndProcess
	if s.fm != nil {
		if err := s.fm.Stop(); err != nil {
			return err
		}
	}
	s.fm = nil

	// 2. cancel 后台 goroutine 的 ctx，唤醒 retryOnFileLockCtx 的 backoff sleep
	s.mutex.Lock()
	cancel := s.decryptCancel
	s.mutex.Unlock()
	if cancel != nil {
		cancel()
	}

	// 3. Wait(5s) 让 inflight goroutine 优雅退出；超时则 warn 但不阻塞切账号
	done := make(chan struct{})
	go func() {
		s.decryptWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Debug().Msg("[autodecrypt] StopAutoDecrypt 所有 goroutine 已退出")
	case <-time.After(stopTimeout):
		log.Warn().
			Dur("timeout", stopTimeout).
			Msg("[autodecrypt] StopAutoDecrypt 等待后台 goroutine 超时，部分任务可能仍在跑")
	}

	return nil
}

func (s *Service) DecryptFileCallback(event fsnotify.Event) error {
	// Local file system
	// WRITE         "/db_storage/message/message_0.db"
	// WRITE         "/db_storage/message/message_0.db"
	// WRITE|CHMOD   "/db_storage/message/message_0.db"
	// Syncthing
	// REMOVE        "/app/data/db_storage/session/session.db"
	// CREATE        "/app/data/db_storage/session/session.db" ← "/app/data/db_storage/session/.syncthing.session.db.tmp"
	// CHMOD         "/app/data/db_storage/session/session.db"
	if !(event.Op.Has(fsnotify.Write) || event.Op.Has(fsnotify.Create)) {
		return nil
	}

	dbFile := s.normalizeDBFile(event.Name)
	isWal := isWalFile(event.Name)
	s.mutex.Lock()
	s.lastEvents[dbFile] = time.Now()
	flags, ok := s.pendingEvents[dbFile]
	if !ok {
		flags = &pendingEvent{}
		s.pendingEvents[dbFile] = flags
	}
	if isWal {
		flags.sawWal = true
	} else {
		flags.sawDB = true
	}

	if !s.pendingActions[dbFile] {
		s.pendingActions[dbFile] = true
		s.mutex.Unlock()
		// wg.Add 必须在 spawn 之前，避免 Stop.Wait 在 Add 之前跑完导致 goroutine 未被追踪
		s.decryptWg.Add(1)
		go func() {
			defer s.decryptWg.Done()
			s.waitAndProcess(dbFile)
		}()
	} else {
		s.mutex.Unlock()
	}

	return nil
}

const (
	decryptRetryAttempts  = 5
	decryptRetryBaseDelay = 5 * time.Second
)

func (s *Service) waitAndProcess(dbFile string) {
	defer s.recoverDecryptPanic("waitAndProcess")
	start := time.Now()
	for {
		debounce := s.getDebounceTimeForFile(dbFile)
		maxWait := s.getMaxWaitTimeForFile(dbFile)
		time.Sleep(debounce)

		s.mutex.Lock()
		lastEventTime := s.lastEvents[dbFile]
		elapsed := time.Since(lastEventTime)
		totalElapsed := time.Since(start)

		if elapsed >= debounce || totalElapsed >= maxWait {
			// 如果是 maxWait 兜底触发（而非安静期达到），说明微信长期活跃，记录警告
			if elapsed < debounce && totalElapsed >= maxWait {
				log.Warn().
					Dur("total_elapsed", totalElapsed).
					Dur("max_wait", maxWait).
					Str("file", dbFile).
					Msg("微信持续活跃超过 maxWait，强制处理积压的文件变更（可能短暂争抢 IO）")
			}
			s.pendingActions[dbFile] = false
			flags := pendingEvent{}
			if state, ok := s.pendingEvents[dbFile]; ok && state != nil {
				flags = *state
			}
			s.pendingEvents[dbFile] = &pendingEvent{}
			s.mutex.Unlock()

			if _, err := os.Stat(dbFile); err != nil {
				return
			}

			// 获取解密槽位，同一时刻只允许 1 个解密任务，降低 IO 争抢
			s.acquireDecryptSlot()
			defer s.releaseDecryptSlot()

			log.Debug().Msgf("Processing file: %s", dbFile)
			workCopyExists := false
			if s.conf.GetWorkDir() != "" {
				if relPath, err := filepath.Rel(s.conf.GetDataDir(), dbFile); err == nil {
					output := filepath.Join(s.conf.GetWorkDir(), relPath)
					if _, err := os.Stat(output); err == nil {
						workCopyExists = true
					}
				}
			}
			if flags.sawDB {
				if flags.sawWal && workCopyExists {
					// Both DB and WAL changed, try incremental first
					handled, err := s.retryDecrypt(func() (bool, error) {
						return s.IncrementalDecryptDBFile(dbFile)
					})
					if err != nil {
						s.handleDecryptError(err)
						return
					}
					if handled {
						return
					}
				}
				// Full re-decrypt: new file, checkpoint update, or incremental failed
				err := retryOnFileLockCtx(s.decryptCtx, func() error {
					return s.DecryptDBFile(dbFile)
				}, decryptRetryAttempts, decryptRetryBaseDelay)
				if err != nil {
					s.handleDecryptError(err)
				}
				return
			}
			if flags.sawWal {
				handled, err := s.retryDecrypt(func() (bool, error) {
					return s.IncrementalDecryptDBFile(dbFile)
				})
				if err != nil {
					s.handleDecryptError(err)
					return
				}
				if handled {
					return
				}
				if !workCopyExists {
					err := retryOnFileLock(func() error {
						return s.DecryptDBFile(dbFile)
					}, decryptRetryAttempts, decryptRetryBaseDelay)
					if err != nil {
						s.handleDecryptError(err)
					}
				}
				return
			}
			if !s.conf.GetWalEnabled() || !workCopyExists {
				err := retryOnFileLockCtx(s.decryptCtx, func() error {
					return s.DecryptDBFile(dbFile)
				}, decryptRetryAttempts, decryptRetryBaseDelay)
				if err != nil {
					s.handleDecryptError(err)
				}
			}
			return
		}
		s.mutex.Unlock()
	}
}

// retryDecrypt wraps IncrementalDecryptDBFile-style functions with file lock retry.
// 使用 Service.decryptCtx 让 Stop() 能 cancel 长 backoff sleep。
func (s *Service) retryDecrypt(op func() (bool, error)) (bool, error) {
	var handled bool
	err := retryOnFileLockCtx(s.decryptCtx, func() error {
		var e error
		handled, e = op()
		return e
	}, decryptRetryAttempts, decryptRetryBaseDelay)
	return handled, err
}

func (s *Service) DecryptDBFile(dbFile string) error {

	decryptor, err := decrypt.NewDecryptor(s.conf.GetPlatform(), s.conf.GetVersion())
	if err != nil {
		return err
	}

	relPath, err := filepath.Rel(s.conf.GetDataDir(), dbFile)
	if err != nil {
		return fmt.Errorf("failed to get relative path for %s: %w", dbFile, err)
	}
	output := filepath.Join(s.conf.GetWorkDir(), relPath)
	if err := util.PrepareDir(filepath.Dir(output)); err != nil {
		return err
	}

	outputTemp := output + ".tmp"
	outputFile, err := os.Create(outputTemp)
	if err != nil {
		return fmt.Errorf("failed to create output file: %v", err)
	}
	defer func() {
		outputFile.Close()
		if err := os.Rename(outputTemp, output); err != nil {
			log.Debug().Err(err).Msgf("failed to rename %s to %s", outputTemp, output)
		}
	}()

	if err := decryptor.Decrypt(context.Background(), dbFile, s.conf.GetDataKey(), outputFile); err != nil {
		if err == errors.ErrAlreadyDecrypted {
			if data, err := util.ReadFileShared(dbFile); err == nil {
				outputFile.Write(data)
			}
			if s.conf.GetWalEnabled() {
				// Remove WAL files if they exist to prevent SQLite from reading encrypted WALs
				s.removeWalFiles(output)
			}
			return nil
		}
		log.Err(err).Msgf("failed to decrypt %s", dbFile)
		return err
	}

	log.Debug().Msgf("Decrypted %s to %s", dbFile, output)

	if s.conf.GetWalEnabled() {
		// Remove WAL files if they exist to prevent SQLite from reading encrypted WALs
		s.removeWalFiles(output)
	}

	return nil
}

func (s *Service) removeWalFiles(dbFile string) {
	walFile := dbFile + "-wal"
	shmFile := dbFile + "-shm"
	if err := os.Remove(walFile); err != nil && !os.IsNotExist(err) {
		log.Debug().Err(err).Msgf("failed to remove wal file %s", walFile)
	}
	if err := os.Remove(shmFile); err != nil && !os.IsNotExist(err) {
		log.Debug().Err(err).Msgf("failed to remove shm file %s", shmFile)
	}
}

func (s *Service) getDebounceTime() time.Duration {
	debounce := s.conf.GetAutoDecryptDebounce()
	if debounce <= 0 {
		return DebounceTime
	}
	return time.Duration(debounce) * time.Millisecond
}

// getMaxWaitTime 返回"即使微信从未安静也至少隔多久强制跑一次"的兜底时长。
// 统一回退到 MaxWaitTime 默认值；不再针对 WAL 模式做 3 秒硬上限。
func (s *Service) getMaxWaitTime() time.Duration {
	return MaxWaitTime
}

// getDebounceTimeForFile 返回指定 DB 文件的 debounce 时长。
// 所有 DB 文件统一使用配置值，不再针对 message_*.db / session.db 等
// "实时 DB" 做 300ms 特殊加速——在后台长期运行场景下，加速反而会抢微信 IO。
func (s *Service) getDebounceTimeForFile(dbFile string) time.Duration {
	return s.getDebounceTime()
}

// getMaxWaitTimeForFile 返回指定 DB 文件的 maxWait 时长。
// 所有 DB 文件统一使用 MaxWaitTime；不再对实时 DB 做 1 秒特殊上限。
func (s *Service) getMaxWaitTimeForFile(dbFile string) time.Duration {
	return s.getMaxWaitTime()
}

func (s *Service) normalizeDBFile(path string) string {
	if strings.HasSuffix(path, ".db-wal") {
		return strings.TrimSuffix(path, "-wal")
	}
	if strings.HasSuffix(path, ".db-shm") {
		return strings.TrimSuffix(path, "-shm")
	}
	return path
}

func isWalFile(path string) bool {
	return strings.HasSuffix(path, ".db-wal") || strings.HasSuffix(path, ".db-shm")
}

func (s *Service) DecryptDBFiles() error {
	// 同 StartAutoDecrypt：rootDir 窄化到 db_storage 避免 fs.WalkDir 遍历
	// 整个 data dir (msg/attach/ 9.6 万图片) 导致 10+ 秒 overhead。
	dbStorage := filepath.Join(s.conf.GetDataDir(), "db_storage")
	dbGroup, err := filemonitor.NewFileGroup("wechat", dbStorage, `.*\.db$`, []string{"fts"})
	if err != nil {
		return err
	}

	dbFiles, err := dbGroup.List()
	if err != nil {
		return err
	}
	sort.SliceStable(dbFiles, func(i, j int) bool {
		pi := dbFilePriority(dbFiles[i])
		pj := dbFilePriority(dbFiles[j])
		if pi != pj {
			return pi < pj
		}
		return filepath.Base(dbFiles[i]) < filepath.Base(dbFiles[j])
	})

	var lastErr error
	failCount := 0

	for _, dbFile := range dbFiles {
		if err := s.DecryptDBFile(dbFile); err != nil {
			log.Debug().Msgf("DecryptDBFile %s failed: %v", dbFile, err)
			lastErr = err
			failCount++
			continue
		}
	}

	if len(dbFiles) > 0 && failCount == len(dbFiles) {
		return fmt.Errorf("decryption failed for all %d files, last error: %w", len(dbFiles), lastErr)
	}

	return nil
}

func dbFilePriority(path string) int {
	base := filepath.Base(path)
	if strings.HasPrefix(base, "message_") && strings.HasSuffix(base, ".db") {
		return 0
	}
	if base == "session.db" {
		return 1
	}
	return 2
}

func (s *Service) IncrementalDecryptDBFile(dbFile string) (bool, error) {
	walPath := dbFile + "-wal"
	if _, err := os.Stat(walPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	relPath, err := filepath.Rel(s.conf.GetDataDir(), dbFile)
	if err != nil {
		return false, fmt.Errorf("failed to get relative path for %s: %w", dbFile, err)
	}
	output := filepath.Join(s.conf.GetWorkDir(), relPath)
	if _, err := os.Stat(output); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	decryptor, err := decrypt.NewDecryptor(s.conf.GetPlatform(), s.conf.GetVersion())
	if err != nil {
		return true, err
	}

	dbInfo, err := common.OpenDBFile(dbFile, decryptor.GetPageSize())
	if err != nil {
		if err == errors.ErrAlreadyDecrypted {
			return false, nil
		}
		return true, err
	}

	keyBytes, err := hex.DecodeString(s.conf.GetDataKey())
	if err != nil {
		return true, errors.DecodeKeyFailed(err)
	}
	if !decryptor.Validate(dbInfo.FirstPage, keyBytes) {
		return true, errors.ErrDecryptIncorrectKey
	}

	encKey, macKey, err := decryptor.DeriveKeys(keyBytes, dbInfo.Salt)
	if err != nil {
		return true, err
	}

	walFile, err := util.OpenFileShared(walPath)
	if err != nil {
		return true, err
	}
	defer walFile.Close()

	info, err := walFile.Stat()
	if err != nil {
		return true, err
	}
	if info.Size() < walHeaderSize {
		return false, nil
	}

	headerBuf := make([]byte, walHeaderSize)
	if _, err := io.ReadFull(walFile, headerBuf); err != nil {
		return true, err
	}
	order, pageSize, salt1, salt2, err := parseWalHeader(headerBuf)
	if err != nil {
		return true, err
	}
	if pageSize != 0 && pageSize != uint32(decryptor.GetPageSize()) {
		return true, fmt.Errorf("unexpected wal page size: %d", pageSize)
	}

	s.mutex.Lock()
	state := s.walStates[dbFile]
	if state != nil && (state.salt1 != salt1 || state.salt2 != salt2 || info.Size() < state.offset) {
		delete(s.walStates, dbFile)
		state = nil
	}
	startOffset := int64(walHeaderSize)
	if state != nil && state.offset > startOffset {
		startOffset = state.offset
	}
	s.mutex.Unlock()

	if _, err := walFile.Seek(startOffset, io.SeekStart); err != nil {
		return true, err
	}

	outputFile, err := os.OpenFile(output, os.O_RDWR, 0)
	if err != nil {
		return true, err
	}
	defer outputFile.Close()

	frameHeader := make([]byte, walFrameHeaderSize)
	pageBuf := make([]byte, decryptor.GetPageSize())
	txFrames := make([]walFrame, 0)
	var lastCommitOffset int64
	var applied bool
	curOffset := startOffset

	for curOffset+int64(walFrameHeaderSize)+int64(decryptor.GetPageSize()) <= info.Size() {
		if _, err := io.ReadFull(walFile, frameHeader); err != nil {
			break
		}
		curOffset += int64(walFrameHeaderSize)

		frameSalt1 := order.Uint32(frameHeader[8:12])
		frameSalt2 := order.Uint32(frameHeader[12:16])
		if frameSalt1 != salt1 || frameSalt2 != salt2 {
			s.mutex.Lock()
			delete(s.walStates, dbFile)
			s.mutex.Unlock()
			return false, nil
		}

		if _, err := io.ReadFull(walFile, pageBuf); err != nil {
			break
		}
		curOffset += int64(decryptor.GetPageSize())

		pageNo := order.Uint32(frameHeader[0:4])
		commit := order.Uint32(frameHeader[4:8])
		data := make([]byte, len(pageBuf))
		copy(data, pageBuf)
		txFrames = append(txFrames, walFrame{pageNo: pageNo, data: data})

		if commit != 0 {
			if err := applyWalFrames(outputFile, txFrames, decryptor, encKey, macKey); err != nil {
				return true, err
			}
			txFrames = txFrames[:0]
			lastCommitOffset = curOffset
			applied = true
		}
	}

	if lastCommitOffset > 0 {
		s.mutex.Lock()
		s.walStates[dbFile] = &walState{
			offset: lastCommitOffset,
			salt1:  salt1,
			salt2:  salt2,
		}
		s.mutex.Unlock()
	}

	// Remove WAL files if they exist to prevent SQLite from reading encrypted WALs
	s.removeWalFiles(output)

	if applied {
		return true, nil
	}
	return true, nil
}

func parseWalHeader(buf []byte) (binary.ByteOrder, uint32, uint32, uint32, error) {
	if len(buf) < walHeaderSize {
		return nil, 0, 0, 0, fmt.Errorf("wal header too short")
	}
	magic := binary.BigEndian.Uint32(buf[0:4])
	var order binary.ByteOrder
	switch magic {
	case 0x377f0682:
		order = binary.BigEndian
	case 0x377f0683:
		order = binary.LittleEndian
	default:
		return nil, 0, 0, 0, fmt.Errorf("invalid wal magic: %x", magic)
	}
	pageSize := order.Uint32(buf[8:12])
	salt1 := order.Uint32(buf[16:20])
	salt2 := order.Uint32(buf[20:24])
	if pageSize == 0 {
		pageSize = 65536
	}
	return order, pageSize, salt1, salt2, nil
}

func applyWalFrames(output *os.File, frames []walFrame, decryptor decrypt.Decryptor, encKey, macKey []byte) error {
	pageSize := decryptor.GetPageSize()
	reserve := decryptor.GetReserve()
	hmacSize := decryptor.GetHMACSize()
	hashFunc := decryptor.GetHashFunc()
	for _, frame := range frames {
		pageNo := int64(frame.pageNo) - 1
		if pageNo < 0 {
			continue
		}
		allZeros := true
		for _, b := range frame.data {
			if b != 0 {
				allZeros = false
				break
			}
		}
		var pageData []byte
		if allZeros {
			pageData = frame.data
		} else {
			decrypted, err := common.DecryptPage(frame.data, encKey, macKey, pageNo, hashFunc, hmacSize, reserve, pageSize)
			if err != nil {
				return err
			}
			if pageNo == 0 {
				fullPage := make([]byte, pageSize)
				copy(fullPage, []byte(common.SQLiteHeader))
				copy(fullPage[len(common.SQLiteHeader):], decrypted)
				pageData = fullPage
			} else {
				pageData = decrypted
			}
		}
		if _, err := output.WriteAt(pageData, pageNo*int64(pageSize)); err != nil {
			return err
		}
	}
	return nil
}

const (
	walHeaderSize      = 32
	walFrameHeaderSize = 24
)
