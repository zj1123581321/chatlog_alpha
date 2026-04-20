package chatlog

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sjzar/chatlog/internal/chatlog/conf"
	"github.com/sjzar/chatlog/internal/chatlog/ctx"
	"github.com/sjzar/chatlog/internal/chatlog/database"
	"github.com/sjzar/chatlog/internal/chatlog/http"
	"github.com/sjzar/chatlog/internal/chatlog/wechat"
	"github.com/sjzar/chatlog/internal/model"
	iwechat "github.com/sjzar/chatlog/internal/wechat"
	"github.com/sjzar/chatlog/pkg/config"
	"github.com/sjzar/chatlog/pkg/util"
	"github.com/sjzar/chatlog/pkg/util/dat2img"
)

// Manager 管理聊天日志应用
type Manager struct {
	ctx *ctx.Context
	sc  *conf.ServerConfig
	scm *config.Manager

	// Services
	db     *database.Service
	http   *http.Service
	wechat *wechat.Service

	// Terminal UI
	app *App
}

func New() *Manager {
	return &Manager{}
}

func (m *Manager) Run(configPath string) error {

	var err error
	m.ctx, err = ctx.New(configPath)
	if err != nil {
		return err
	}

	m.wechat = wechat.NewService(m.ctx)

	m.db = database.NewService(m.ctx)

	m.http = http.NewService(m.ctx, m.db)

	// 注入 autodecrypt phase/status 查询闭包 —— 用于 HTTP 503 gate 和 /status 接口。
	// Stage H (Codex Tension #1): 首次全量期间 /api/v1/chatlog 等读接口返 503。
	m.http.SetAutoDecryptPhaseFunc(func() string {
		return string(m.wechat.GetPhase())
	})
	m.http.SetAutoDecryptStatusFunc(func() http.AutoDecryptStatus {
		phase := m.wechat.GetPhase()
		status := http.AutoDecryptStatus{
			Enabled: m.ctx.GetAutoDecrypt(),
			Phase:   string(phase),
		}
		if lr := m.wechat.GetLastRun(); lr != nil {
			status.LastRun = &http.AutoDecryptLastRun{
				StartedAt:    lr.StartedAt.Format(time.RFC3339),
				EndedAt:      lr.EndedAt.Format(time.RFC3339),
				DurationSecs: lr.DurationSecs,
				FinalPhase:   string(lr.FinalPhase),
				Error:        lr.Error,
			}
		}
		// 只在 first_full 阶段返回进度（避免 UI 在 idle/live 态显示陈旧进度）
		if phase == wechat.PhaseFirstFull {
			if evt := m.wechat.GetLatestProgress(); evt != nil {
				pct := 0.0
				if evt.BytesTotal > 0 {
					pct = float64(evt.BytesDone) / float64(evt.BytesTotal) * 100
				}
				elapsed := 0.0
				etaStr := ""
				if !evt.StartedAt.IsZero() {
					elapsed = time.Since(evt.StartedAt).Seconds()
					etaStr = wechat.NewETACalculator(evt.StartedAt).Format(evt.BytesDone, evt.BytesTotal)
				}
				currentFile := evt.CurrentFile
				if currentFile != "" {
					currentFile = filepath.Base(currentFile)
				}
				status.Progress = &http.AutoDecryptProgress{
					FilesDone:   evt.FilesDone,
					FilesTotal:  evt.FilesTotal,
					BytesDone:   evt.BytesDone,
					BytesTotal:  evt.BytesTotal,
					Pct:         pct,
					CurrentFile: currentFile,
					ElapsedSecs: elapsed,
					ETA:         etaStr,
				}
			}
		}
		return status
	})

	m.ctx.SetWeChatInstances(m.wechat.GetWeChatInstances())
	instances := m.ctx.GetWeChatInstances()
	if len(instances) >= 1 {
		m.ctx.SwitchCurrent(instances[0])
	}

	if m.ctx.GetHTTPEnabled() {
		// 启动HTTP服务
		if err := m.StartService(); err != nil {
			m.StopService()
		}
	}
	if m.ctx.GetAutoDecrypt() {
		// 重置状态，由后台协程在成功后重新设置
		m.ctx.SetAutoDecrypt(false)
		go func() {
			if err := m.StartAutoDecrypt(StartAutoDecryptOpts{SkipPrecheck: true}); err != nil {
				log.Info().Err(err).Msg("恢复自动解密失败")
				m.ctx.SetAutoDecrypt(false)
			}
			if m.app != nil {
				m.app.QueueUpdateDraw(func() {
					m.app.updateMenuItemsState()
				})
			}
		}()
	}
	// 启动终端UI
	m.app = NewApp(m.ctx, m)
	m.app.Run() // 阻塞
	return nil
}

func (m *Manager) Switch(info *iwechat.Account, history string) error {
	if m.ctx.GetAutoDecrypt() {
		if err := m.StopAutoDecrypt(); err != nil {
			return err
		}
	}
	if m.ctx.GetHTTPEnabled() {
		if err := m.stopService(); err != nil {
			return err
		}
	}
	if info != nil {
		m.ctx.SwitchCurrent(info)
	} else {
		m.ctx.SwitchHistory(history)
	}

	if m.ctx.GetHTTPEnabled() {
		// 启动HTTP服务
		if err := m.StartService(); err != nil {
			log.Info().Err(err).Msg("启动服务失败")
			m.StopService()
		}
	}
	return nil
}

func (m *Manager) StartService() error {

	// 按依赖顺序启动服务
	if err := m.db.Start(); err != nil {
		return err
	}

	if err := m.http.Start(); err != nil {
		m.db.Stop()
		return err
	}

	// 如果是 4.0 版本，更新下 xorkey
	if m.ctx.GetVersion() == 4 {
		dat2img.SetAesKey(m.ctx.GetImgKey())
		go dat2img.ScanAndSetXorKey(m.ctx.GetDataDir())
	}

	// 更新状态
	m.ctx.SetHTTPEnabled(true)

	return nil
}

func (m *Manager) StopService() error {
	if err := m.stopService(); err != nil {
		return err
	}

	// 更新状态
	m.ctx.SetHTTPEnabled(false)

	return nil
}

func (m *Manager) stopService() error {
	// 按依赖的反序停止服务
	var errs []error

	if err := m.http.Stop(); err != nil {
		errs = append(errs, err)
	}

	if err := m.db.Stop(); err != nil {
		errs = append(errs, err)
	}

	// 如果有错误，返回第一个错误
	if len(errs) > 0 {
		return errs[0]
	}

	return nil
}

func (m *Manager) SetHTTPAddr(text string) error {
	var addr string
	if util.IsNumeric(text) {
		addr = fmt.Sprintf("127.0.0.1:%s", text)
	} else if strings.HasPrefix(text, "http://") {
		addr = strings.TrimPrefix(text, "http://")
	} else if strings.HasPrefix(text, "https://") {
		addr = strings.TrimPrefix(text, "https://")
	} else {
		addr = text
	}
	m.ctx.SetHTTPAddr(addr)
	return nil
}

func (m *Manager) GetDataKey() error {
	cur := m.ctx.GetCurrent()
	if cur == nil {
		return fmt.Errorf("未选择任何账号")
	}
	if _, err := m.wechat.GetDataKey(cur); err != nil {
		return err
	}
	m.ctx.Refresh()
	m.ctx.UpdateConfig()
	return nil
}

func (m *Manager) GetImageKey() error {
	cur := m.ctx.GetCurrent()
	if cur == nil {
		return fmt.Errorf("未选择任何账号")
	}
	if _, err := m.wechat.GetImageKey(cur); err != nil {
		return err
	}
	m.ctx.Refresh()
	m.ctx.UpdateConfig()
	return nil
}

func (m *Manager) RestartAndGetDataKey(onStatus func(string)) error {
	cur := m.ctx.GetCurrent()
	if cur == nil {
		return fmt.Errorf("未选择任何账号")
	}

	pid := cur.PID
	exePath := cur.ExePath

	// 1. Terminate the process
	if onStatus != nil {
		onStatus("正在结束微信进程...")
	}
	log.Info().Msgf("Killing WeChat process with PID %d", pid)
	process, err := os.FindProcess(int(pid))
	if err != nil {
		return fmt.Errorf("could not find process with PID %d: %w", pid, err)
	}
	if err := process.Kill(); err != nil {
		return fmt.Errorf("failed to kill process with PID %d: %w", pid, err)
	}

	// 2. Wait for the process to disappear
	log.Info().Msg("Waiting for WeChat process to terminate...")
	for i := 0; i < 10; i++ { // Wait for max 10 seconds
		instances := m.wechat.GetWeChatInstances()
		found := false
		for _, inst := range instances {
			if inst.PID == pid {
				found = true
				break
			}
		}
		if !found {
			break
		}
		time.Sleep(1 * time.Second)
	}

	// 3. Restart WeChat
	if onStatus != nil {
		onStatus("正在重启微信...")
	}
	log.Info().Msgf("Restarting WeChat from %s", exePath)
	cmd := exec.Command(exePath)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to restart WeChat: %w", err)
	}

	// 4. Wait for the new process to appear.
	if onStatus != nil {
		onStatus("正在等待新进程启动...")
	}
	log.Info().Msg("Waiting for new WeChat process to start...")
	var newInstance *iwechat.Account
	for i := 0; i < 30; i++ { // Wait for max 30 seconds
		instances := m.wechat.GetWeChatInstances()
		// Try to find a new instance. A new instance is one with a different PID.
		for _, inst := range instances {
			if inst.PID != pid && inst.ExePath == exePath {
				newInstance = inst
				break
			}
		}
		if newInstance != nil {
			break
		}
		time.Sleep(1 * time.Second)
	}

	if newInstance == nil {
		return fmt.Errorf("failed to find new WeChat process after restart")
	}
	log.Info().Msgf("Found new WeChat process with PID %d", newInstance.PID)

	// 5. Switch to the new instance
	m.ctx.SwitchCurrent(newInstance)

	// 6. Get the key
	// 增加重试逻辑：微信刚启动时可能DLL未加载完成导致Hook失败，需要等待
	log.Info().Msg("Getting key from new WeChat process...")

	// 使用携带回调的 context
	ctx := context.WithValue(context.Background(), "status_callback", onStatus)

	var key, imgKey string

	// 初始化截止时间 (30秒)
	deadline := time.Now().Add(30 * time.Second)

	for {
		if onStatus != nil {
			onStatus("正在等待微信初始化...")
		}

		// 尝试获取密钥 (会尝试初始化DLL)
		curForKey := m.ctx.GetCurrent()
		key, imgKey, err = curForKey.GetKey(ctx)

		// 如果成功，跳出循环
		// 注意：GetKey 成功意味着 Hook 安装成功且已经获取到了密钥
		// 但实际上 DLL 模式下，Extract 会在 Hook 安装成功后阻塞轮询。
		// 所以如果这里返回 nil，说明已经完成了整个流程。
		if err == nil {
			break
		}

		// 如果超时，且包含特定错误，则退出
		if time.Now().After(deadline) {
			return fmt.Errorf("获取密钥超时: %v", err)
		}

		// 检查错误类型，决定是否重试
		// 如果是初始化失败（例如微信模块未加载），则重试
		// 如果是 "wechat process not found" 等临时错误，也重试
		// 如果是 pollKeys 内部的超时（30s），说明 Hook 成功但用户未操作，此时不应该重试，
		// 但 pollKeys 内部超时会返回错误，这里会捕获。
		// 不过 pollKeys 耗时 30s，如果走到这里说明已经等了 30s，外层 deadline 也会触发。
		
		// 只有快速失败（初始化错误）才需要 fast retry
		log.Debug().Err(err).Msg("获取密钥尝试失败，准备重试")
		
		time.Sleep(1 * time.Second)
	}

	m.ctx.SetDataKey(key)
	m.ctx.SetImgKey(imgKey)
	m.ctx.Refresh()
	m.ctx.UpdateConfig()

	log.Info().Msg("Successfully got key from new WeChat process.")
	return nil
}

func (m *Manager) DecryptDBFiles() error {
	if m.ctx.GetDataKey() == "" {
		if m.ctx.GetCurrent() == nil {
			return fmt.Errorf("未选择任何账号")
		}
		if err := m.GetDataKey(); err != nil {
			return err
		}
	}
	if m.ctx.GetWorkDir() == "" {
		m.ctx.SetWorkDir(util.DefaultWorkDir(m.ctx.GetAccount()))
	}

	if err := m.wechat.DecryptDBFiles(); err != nil {
		return err
	}
	m.ctx.Refresh()
	m.ctx.UpdateConfig()
	return nil
}

// StartAutoDecryptOpts 配置 StartAutoDecrypt 的启动行为。
// 替代原先的 skipPrecheck ...bool 可变参数，为后续传入 ctx / ProgressCh 做准备。
type StartAutoDecryptOpts struct {
	// SkipPrecheck 为 true 时跳过启动前预检，依赖运行期熔断兜底。
	// 用于启动恢复场景（autoDecryptRecovery），也用于 UI 按钮路径（Stage G 后）走
	// 秒级单文件预检而非全量。
	SkipPrecheck bool
}

// StartAutoDecrypt 开启自动解密。
//
// Stage G 重构：UI 按钮路径（SkipPrecheck=false）不再阻塞等全量解密，而是：
//  1. 单文件预检（~秒级）验证密钥 → Phase: Idle → Precheck
//  2. 启动文件监听器（~秒级）→ Phase: Precheck → FirstFull
//  3. Fire-and-forget spawn 首次全量 goroutine → 完成后 Phase: FirstFull → Live
//  4. 函数秒级返回，UI modal 立即关闭
//
// Recovery 路径（SkipPrecheck=true）跳过单文件预检，workdir 已有数据直接 Live。
func (m *Manager) StartAutoDecrypt(opts StartAutoDecryptOpts) (retErr error) {
	log.Info().Bool("skip_precheck", opts.SkipPrecheck).Msg("[autodecrypt] Manager.StartAutoDecrypt 入口")
	defer func() {
		log.Info().Err(retErr).Msg("[autodecrypt] Manager.StartAutoDecrypt 返回")
	}()

	if m.ctx.GetDataKey() == "" || m.ctx.GetDataDir() == "" {
		return fmt.Errorf("请先获取密钥")
	}

	// 预检：单文件解密验证密钥（替代原先跑全量 DecryptDBFiles）
	if !opts.SkipPrecheck {
		m.wechat.SetPhase(wechat.PhasePrecheck)
		log.Info().Msg("[autodecrypt] 开始单文件预检")
		dbFile, err := wechat.PickSmallestDBForPrecheck(m.ctx.GetDataDir())
		if err == wechat.ErrNoDBFile {
			// 空 db_storage 目录：降级继续（比如新账号还没初始化）。
			// 运行期熔断会在文件监听到真实 db 写入时兜底捕获密钥错。
			log.Warn().Msg("[autodecrypt] db_storage 为空，跳过预检继续启动")
		} else if err != nil {
			m.wechat.SetPhase(wechat.PhaseFailed)
			return fmt.Errorf("预检文件选择失败: %w", err)
		} else {
			precheckCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := m.wechat.DecryptSingleDBForPrecheck(precheckCtx, dbFile); err != nil {
				m.wechat.SetPhase(wechat.PhaseFailed)
				return fmt.Errorf("预检失败（密钥或环境错）: %w", err)
			}
			log.Info().Str("file", dbFile).Msg("[autodecrypt] 单文件预检通过")
		}
	}

	if m.ctx.GetWorkDir() == "" {
		return fmt.Errorf("请先执行解密数据")
	}

	m.wechat.SetAutoDecryptErrorHandler(func(err error) {
		log.Error().Err(err).Msg("自动解密失败，停止服务")
		m.StopAutoDecrypt()

		if m.app != nil {
			m.app.QueueUpdateDraw(func() {
				m.app.showError(fmt.Errorf("自动解密失败，已停止服务: %v", err))
				m.app.updateMenuItemsState()
			})
		}
	})

	log.Info().Msg("[autodecrypt] 调用 wechat.StartAutoDecrypt（启动文件监控）")
	if err := m.wechat.StartAutoDecrypt(); err != nil {
		m.wechat.SetPhase(wechat.PhaseFailed)
		return err
	}
	log.Info().Msg("[autodecrypt] wechat.StartAutoDecrypt 返回，准备 ctx.SetAutoDecrypt(true)")

	m.ctx.SetAutoDecrypt(true)

	// Stage G: UI 路径 fire-and-forget 首次全量。recovery 路径 workdir 已有数据直接 Live。
	if opts.SkipPrecheck {
		m.wechat.SetPhase(wechat.PhaseLive)
		log.Info().Msg("[autodecrypt] Recovery 路径直接进入 Live phase")
	} else {
		m.wechat.SetPhase(wechat.PhaseFirstFull)
		log.Info().Msg("[autodecrypt] 启动后台 firstFullDecrypt goroutine")
		m.wechat.SpawnFirstFullDecrypt(m.DecryptDBFiles)
	}
	return nil
}

// GetAutoDecryptPhase 返回当前自动解密 lifecycle phase。
// 供 TUI 状态栏 / 其他需要根据 phase 分支 UI 的消费者读取。
func (m *Manager) GetAutoDecryptPhase() wechat.AutoDecryptPhase {
	return m.wechat.GetPhase()
}

// GetAutoDecryptProgress 返回首次全量解密的最新进度快照。
// 非 first_full 阶段可能返回 nil（取决于 publisher 状态）。
func (m *Manager) GetAutoDecryptProgress() *wechat.ProgressEvent {
	return m.wechat.GetLatestProgress()
}

func (m *Manager) StopAutoDecrypt() error {
	if err := m.wechat.StopAutoDecrypt(); err != nil {
		return err
	}

	m.ctx.SetAutoDecrypt(false)
	return nil
}

func (m *Manager) RefreshSession() error {
	if m.db.GetDB() == nil {
		if err := m.db.Start(); err != nil {
			return err
		}
	}
	resp, err := m.db.GetSessions("", 1, 0)
	if err != nil {
		return err
	}
	if len(resp.Items) == 0 {
		return nil
	}
	m.ctx.SetLastSession(resp.Items[0].NTime)
	return nil
}

func (m *Manager) GetLatestSession() (*model.Session, error) {
	if m.db == nil || m.db.GetDB() == nil {
		return nil, nil
	}
	resp, err := m.db.GetSessions("", 1, 0)
	if err != nil {
		return nil, err
	}
	if len(resp.Items) > 0 {
		return resp.Items[0], nil
	}
	return nil, nil
}



func (m *Manager) CommandKey(configPath string, pid int, force bool, showXorKey bool) (string, error) {

	var err error
	m.ctx, err = ctx.New(configPath)
	if err != nil {
		return "", err
	}

	m.wechat = wechat.NewService(m.ctx)

	m.ctx.SetWeChatInstances(m.wechat.GetWeChatInstances())
	allInstances := m.ctx.GetWeChatInstances()
	if len(allInstances) == 0 {
		return "", fmt.Errorf("wechat process not found")
	}

	if len(allInstances) == 1 {
		// 确保当前账户已设置
		if m.ctx.GetCurrent() == nil {
			m.ctx.SwitchCurrent(allInstances[0])
		}

		key, imgKey := m.ctx.GetDataKey(), m.ctx.GetImgKey()
		if len(key) == 0 || len(imgKey) == 0 || force {
			key, imgKey, err = allInstances[0].GetKey(context.Background())
			if err != nil {
				return "", err
			}
			m.ctx.Refresh()
			m.ctx.UpdateConfig()
		}

		result := fmt.Sprintf("Data Key: [%s]\nImage Key: [%s]", key, imgKey)
		if m.ctx.GetVersion() == 4 && showXorKey {
			if b, err := dat2img.ScanAndSetXorKey(m.ctx.GetDataDir()); err == nil {
				result += fmt.Sprintf("\nXor Key: [0x%X]", b)
			}
		}

		return result, nil
	}
	if pid == 0 {
		str := "Select a process:\n"
		for _, ins := range allInstances {
			str += fmt.Sprintf("PID: %d. %s[Version: %s Data Dir: %s ]\n", ins.PID, ins.Name, ins.FullVersion, ins.DataDir)
		}
		return str, nil
	}
	for _, ins := range allInstances {
		if ins.PID == uint32(pid) {
			// 确保当前账户已设置
			cur := m.ctx.GetCurrent()
			if cur == nil || cur.PID != ins.PID {
				m.ctx.SwitchCurrent(ins)
			}

			key, imgKey := ins.Key, ins.ImgKey
			if len(key) == 0 || len(imgKey) == 0 || force {
				key, imgKey, err = ins.GetKey(context.Background())
				if err != nil {
					return "", err
				}
				m.ctx.Refresh()
				m.ctx.UpdateConfig()
			}
			result := fmt.Sprintf("Data Key: [%s]\nImage Key: [%s]", key, imgKey)
			if m.ctx.GetVersion() == 4 && showXorKey {
				if b, err := dat2img.ScanAndSetXorKey(m.ctx.GetDataDir()); err == nil {
					result += fmt.Sprintf("\nXor Key: [0x%X]", b)
				}
			}
			return result, nil
		}
	}
	return "", fmt.Errorf("wechat process not found")
}

func (m *Manager) CommandDecrypt(configPath string, cmdConf map[string]any) error {

	var err error
	m.sc, m.scm, err = conf.LoadServiceConfig(configPath, cmdConf)
	if err != nil {
		return err
	}

	dataDir := m.sc.GetDataDir()
	if len(dataDir) == 0 {
		return fmt.Errorf("dataDir is required")
	}

	dataKey := m.sc.GetDataKey()
	if len(dataKey) == 0 {
		return fmt.Errorf("dataKey is required")
	}

	m.wechat = wechat.NewService(m.sc)

	if err := m.wechat.DecryptDBFiles(); err != nil {
		return err
	}

	return nil
}

func (m *Manager) CommandHTTPServer(configPath string, cmdConf map[string]any) error {

	var err error
	m.sc, m.scm, err = conf.LoadServiceConfig(configPath, cmdConf)
	if err != nil {
		return err
	}

	dataDir := m.sc.GetDataDir()
	workDir := m.sc.GetWorkDir()
	if len(dataDir) == 0 && len(workDir) == 0 {
		return fmt.Errorf("dataDir or workDir is required")
	}

	dataKey := m.sc.GetDataKey()
	if len(dataKey) == 0 {
		return fmt.Errorf("dataKey is required")
	}

	// 如果是 4.0 版本，处理图片密钥
	version := m.sc.GetVersion()
	if version == 4 && len(dataDir) != 0 {
		dat2img.SetAesKey(m.sc.GetImgKey())
		go dat2img.ScanAndSetXorKey(dataDir)
	}

	log.Info().Msgf("server config: %+v", m.sc)

	m.wechat = wechat.NewService(m.sc)

	m.db = database.NewService(m.sc)

	m.http = http.NewService(m.sc, m.db)

	if m.sc.GetAutoDecrypt() {
		if err := m.wechat.StartAutoDecrypt(); err != nil {
			return err
		}
		log.Info().Msg("auto decrypt is enabled")
	}

	// init db
	go func() {
		// 如果工作目录为空，则解密数据
		if entries, err := os.ReadDir(workDir); err == nil && len(entries) == 0 {
			log.Info().Msgf("work dir is empty, decrypt data.")
			m.db.SetDecrypting()
			if err := m.wechat.DecryptDBFiles(); err != nil {
				log.Info().Msgf("decrypt data failed: %v", err)
				return
			}
			log.Info().Msg("decrypt data success")
		}

		// 按依赖顺序启动服务
		if err := m.db.Start(); err != nil {
			log.Info().Msgf("start db failed, try to decrypt data.")
			m.db.SetDecrypting()
			if err := m.wechat.DecryptDBFiles(); err != nil {
				log.Info().Msgf("decrypt data failed: %v", err)
				return
			}
			log.Info().Msg("decrypt data success")
			if err := m.db.Start(); err != nil {
				log.Info().Msgf("start db failed: %v", err)
				m.db.SetError(err.Error())
				return
			}
		}
	}()

	return m.http.ListenAndServe()
}

