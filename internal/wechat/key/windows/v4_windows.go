package windows

import (
	"context"
	"encoding/hex"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/rs/zerolog/log"
	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/windows"

	"github.com/sjzar/chatlog/internal/errors"
	"github.com/sjzar/chatlog/internal/wechat/decrypt"
	"github.com/sjzar/chatlog/internal/wechat/model"
)

const (
	MEM_PRIVATE = 0x20000
	MEM_MAPPED  = 0x40000
	MEM_IMAGE   = 0x1000000
	MaxWorkers  = 8
)

func (e *V4Extractor) Extract(ctx context.Context, proc *model.Process) (string, string, error) {
	// 图片密钥扫描强依赖 dataDir 以及验证样本（*_t.dat / 模板文件），需要登录成功并浏览图片后才能就绪。
	// 因此：dataDir 未就绪时直接返回，让上层负责等待/重试，避免启动无效内存扫描。
	if proc.DataDir == "" {
		return "", "", fmt.Errorf("数据目录未就绪，无法进行图片密钥扫描，请确保微信已登录")
	}

	// 收集所有 Weixin 进程的句柄（密钥可能在任意子进程中）
	allPIDs := e.findAllWeixinPIDs(proc.PID)
	var handles []windows.Handle
	for _, pid := range allPIDs {
		h, err := windows.OpenProcess(windows.PROCESS_VM_READ|windows.PROCESS_QUERY_INFORMATION, false, pid)
		if err != nil {
			log.Debug().Err(err).Msgf("无法打开进程 %d", pid)
			continue
		}
		handles = append(handles, h)
	}
	if len(handles) == 0 {
		// 降级: 只扫描主进程
		h, err := windows.OpenProcess(windows.PROCESS_VM_READ|windows.PROCESS_QUERY_INFORMATION, false, proc.PID)
		if err != nil {
			return "", "", errors.OpenProcessFailed(err)
		}
		handles = append(handles, h)
	}
	defer func() {
		for _, h := range handles {
			windows.CloseHandle(h)
		}
	}()
	log.Info().Msgf("将扫描 %d 个 Weixin 进程 (PIDs: %v)", len(allPIDs), allPIDs)

	// 设置总超时时间：60秒
	// 这给用户足够的时间去打开图片
	timeout := time.After(60 * time.Second)
	scanRound := 0
	waitTick := 0

	for {
		// 确保图片验证样本就绪：如果用户刚登录/刚打开图片，*_t.dat 可能是运行中生成的
		// 这里按 1s 轮询尝试构建“仅图片验证”的验证器，样本就绪后才进入真正的内存扫描。
		if e.validator == nil || !e.validator.ImgKeyReady() {
			// 尝试用 dataDir 重新加载验证样本（不依赖数据库文件存在）
			if v, _ := decrypt.NewImgKeyOnlyValidator(proc.Platform, proc.Version, proc.DataDir); v != nil {
				e.validator = v
			}

			// 样本仍未就绪：提示用户打开图片触发缓存生成
			if e.validator == nil || !e.validator.ImgKeyReady() {
				if waitTick == 0 || waitTick%5 == 0 {
					msg := "图片密钥验证样本未就绪：请确保微信已登录，并打开任意图片以生成缓存文件（*_t.dat）后再继续"
					log.Info().Msg(msg)
					if e.logger != nil {
						e.logger.LogInfo(msg)
					}
				}
				select {
				case <-ctx.Done():
					return "", "", ctx.Err()
				case <-timeout:
					return "", "", fmt.Errorf("获取图片密钥超时(60秒)：验证样本未就绪，请登录微信并打开图片后重试")
				case <-time.After(1 * time.Second):
					waitTick++
					continue
				}
			}
		}

		scanRound++
		// Create context to control all goroutines for THIS round
		scanCtx, cancel := context.WithCancel(ctx)
		
		// 记录提示日志
		if scanRound == 1 || scanRound % 5 == 0 {
			msg := fmt.Sprintf("正在进行第 %d 轮内存扫描... 请打开任意图片以触发密钥加载", scanRound)
			log.Info().Msg(msg)
			if e.logger != nil {
				e.logger.LogInfo(msg)
			}
		}

		// Create channels for memory data and results
		memoryChannel := make(chan []byte, 100)
		resultChannel := make(chan [2]string, 1)

		// Determine number of worker goroutines
		workerCount := runtime.NumCPU()
		if workerCount < 2 {
			workerCount = 2
		}
		if workerCount > MaxWorkers {
			workerCount = MaxWorkers
		}

		// Start consumer goroutines
		var workerWaitGroup sync.WaitGroup
		workerWaitGroup.Add(workerCount)
		for index := 0; index < workerCount; index++ {
			go func() {
				defer workerWaitGroup.Done()
				e.worker(scanCtx, handles[0], memoryChannel, resultChannel)
			}()
		}

		// Start producer goroutines - 为每个进程启动一个 producer
		var producerWaitGroup sync.WaitGroup
		producerWaitGroup.Add(len(handles))
		for _, h := range handles {
			go func(handle windows.Handle) {
				defer producerWaitGroup.Done()
				err := e.findMemory(scanCtx, handle, memoryChannel)
				if err != nil {
					log.Debug().Err(err).Msg("Failed to find memory regions")
				}
			}(h)
		}
		// 等所有 producer 完成后关闭 channel
		go func() {
			producerWaitGroup.Wait()
			close(memoryChannel)
		}()

		// Wait for producer and consumers to complete IN BACKGROUND
		// We need this to close resultChannel
		go func() {
			producerWaitGroup.Wait()
			workerWaitGroup.Wait()
			close(resultChannel)
		}()

		// Wait for result of THIS round
		var roundImgKey string
		var roundDone bool

		for !roundDone {
			select {
			case <-ctx.Done():
				cancel()
				return "", "", ctx.Err()
			case <-timeout:
				cancel()
				return "", "", fmt.Errorf("获取图片密钥超时(60秒)，请确保已打开图片")
			case result, ok := <-resultChannel:
				if !ok {
					// Channel closed, round finished
					roundDone = true
					break
				}
				// Found something?
				if result[1] != "" {
					roundImgKey = result[1]
					// Found it!
					cancel()
					return "", roundImgKey, nil
				}
			}
		}
		
		cancel() // Ensure cleanup of this round

		// If we are here, round finished but no key found.
		// Wait a bit before next round
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-timeout:
			return "", "", fmt.Errorf("获取图片密钥超时(60秒)，请确保已打开图片")
		case <-time.After(1 * time.Second):
			// Continue to next round
		}
	}
}

// findMemoryV4 searches for writable memory regions for V4 version
func (e *V4Extractor) findMemory(ctx context.Context, handle windows.Handle, memoryChannel chan<- []byte) error {
	// Define search range
	minAddr := uintptr(0x10000)    // Process space usually starts from 0x10000
	maxAddr := uintptr(0x7FFFFFFF) // 32-bit process space limit

	if runtime.GOARCH == "amd64" {
		maxAddr = uintptr(0x7FFFFFFFFFFF) // 64-bit process space limit
	}
	log.Debug().Msgf("Scanning memory regions from 0x%X to 0x%X", minAddr, maxAddr)

	currentAddr := minAddr

	for currentAddr < maxAddr {
		var memInfo windows.MemoryBasicInformation
		err := windows.VirtualQueryEx(handle, currentAddr, &memInfo, unsafe.Sizeof(memInfo))
		if err != nil {
			break
		}

		// 扫描所有已提交的可读内存（MEM_PRIVATE + MEM_MAPPED + MEM_IMAGE）
		isReadable := (memInfo.Protect&windows.PAGE_NOACCESS) == 0 && (memInfo.Protect&windows.PAGE_GUARD) == 0
		isTargetType := memInfo.Type == MEM_PRIVATE || memInfo.Type == MEM_MAPPED || memInfo.Type == MEM_IMAGE
		if memInfo.State == windows.MEM_COMMIT && isReadable && isTargetType {
			regionSize := uintptr(memInfo.RegionSize)
			if currentAddr+regionSize > maxAddr {
				regionSize = maxAddr - currentAddr
			}

			memory := make([]byte, regionSize)
			if err = windows.ReadProcessMemory(handle, currentAddr, &memory[0], regionSize, nil); err == nil {
				select {
				case memoryChannel <- memory:
				case <-ctx.Done():
					return nil
				}
			}
		}

		// Move to next memory region
		currentAddr = uintptr(memInfo.BaseAddress) + uintptr(memInfo.RegionSize)
	}

	return nil
}

// worker processes memory regions to find V4 version key
func (e *V4Extractor) worker(ctx context.Context, handle windows.Handle, memoryChannel <-chan []byte, resultChannel chan<- [2]string) {
	// Data Key scanning logic removed as per requirement.
	// Native scanner is now exclusively for Image Key (Dart mode).

	// 匹配上游 wx_key: 支持大小写字母+数字
	isAlphaNum := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
	}

	var imgKey string

	// tryValidateAndReport 尝试验证候选密钥并报告结果
	// candidate 是 16 字节的 ASCII 字符串，直接用作 AES-128 key
	tryValidateAndReport := func(candidate []byte, mode string) bool {
		if e.validator.ValidateImgKey(candidate) {
			// 返回 ASCII 字符串形式的密钥（与上游 find_image_key.py 一致）
			imgKey = string(candidate)
			msg := fmt.Sprintf("找到图片密钥(%s)! Key: %s", mode, imgKey)
			log.Info().Msg(msg)
			if e.logger != nil {
				e.logger.LogStatus(1, msg)
			}
			select {
			case resultChannel <- [2]string{"", imgKey}:
			case <-ctx.Done():
			}
			return true
		}
		return false
	}

	for {
		select {
		case <-ctx.Done():
			return
		case memory, ok := <-memoryChannel:
			if !ok {
				if imgKey != "" {
					select {
					case resultChannel <- [2]string{"", imgKey}:
					default:
					}
				}
				return
			}

			if imgKey != "" {
				continue
			}
			if e.validator == nil {
				continue
			}

			// === 匹配 wechat-decrypt 的 _scan_regions 逻辑 ===
			// 扫描所有连续字母数字序列，同时匹配32字符和16字符

			i := 0
			for i <= len(memory)-16 {
				if !isAlphaNum(memory[i]) {
					i++
					continue
				}
				// 前边界检查
				if i > 0 && isAlphaNum(memory[i-1]) {
					i++
					continue
				}

				// 计算连续字母数字序列的长度
				seqLen := 0
				for j := i; j < len(memory) && isAlphaNum(memory[j]); j++ {
					seqLen++
					if seqLen > 64 { // 防止超长序列浪费时间
						break
					}
				}

				// 精确 32 字符: 先试前16字节作AES-128 key，再试完整32字节
				if seqLen == 32 {
					candidate32 := memory[i : i+32]
					// 前16字节作为AES-128 key (wechat-decrypt 的主要逻辑)
					if tryValidateAndReport(candidate32[:16], "32char-first16") {
						return
					}
					// 完整32字节作为AES-256 key
					if tryValidateAndReport(candidate32, "32char-full") {
						return
					}
				}

				// 精确 16 字符
				if seqLen == 16 {
					candidate16 := memory[i : i+16]
					if tryValidateAndReport(candidate16, "16char") {
						return
					}
				}

				i += seqLen
				if seqLen == 0 {
					i++
				}
			}
		}
	}
}

// validateKey validates a single key candidate and returns the key and whether it's an image key
func (e *V4Extractor) validateKey(handle windows.Handle, addr uint64) (string, bool) {
	if e.validator == nil {
		return "", false
	}

	keyData := make([]byte, 0x20) // 32-byte key
	if err := windows.ReadProcessMemory(handle, uintptr(addr), &keyData[0], uintptr(len(keyData)), nil); err != nil {
		return "", false
	}

	// Data Key validation removed.
	
	// Only check if it's a valid image key
	if e.validator.ValidateImgKey(keyData) {
		return hex.EncodeToString(keyData[:16]), true // Image key
	}

	return "", false
}

// findAllWeixinPIDs 查找所有 Weixin 进程的 PID（包括子进程）
func (e *V4Extractor) findAllWeixinPIDs(mainPID uint32) []uint32 {
	pids := []uint32{mainPID}
	seen := map[uint32]bool{mainPID: true}

	processes, err := process.Processes()
	if err != nil {
		return pids
	}

	for _, p := range processes {
		name, err := p.Name()
		if err != nil {
			continue
		}
		name = strings.TrimSuffix(name, ".exe")
		if name == "Weixin" {
			pid := uint32(p.Pid)
			if !seen[pid] {
				seen[pid] = true
				pids = append(pids, pid)
			}
		}
	}
	return pids
}