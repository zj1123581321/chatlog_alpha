package windows

import (
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/sjzar/chatlog/internal/wechat/model"
	"github.com/sjzar/chatlog/pkg/appver"
)

const (
	V4ProcessName = "Weixin"
	V4DBFile      = `db_storage\session\session.db`

	// ProbeCacheTTL 控制对同一 Weixin PID 的 initializeProcessInfo 重探测节流。
	// 一旦缓存命中（DataDir 非空）就在此窗口内复用，不再调 gopsutil p.OpenFiles()。
	// TUI 每秒 tick 一次 FindProcesses，不加节流会每秒泄漏一个 Process HANDLE
	// （gopsutil v4.25.7 OpenFilesWithContext 漏关 OpenProcess 返回的 handle）。
	ProbeCacheTTL = 60 * time.Second
)

// cacheEntry 保存一个 Weixin PID 的已探测 ProcessInfo + 最近探测时间戳。
type cacheEntry struct {
	info       *model.Process
	lastProbed time.Time
}

// Detector 实现 Windows 平台的进程检测器，内部用 PID → ProcessInfo 缓存
// 抑制对 gopsutil p.OpenFiles() 的高频调用。
type Detector struct {
	mu    sync.Mutex
	cache map[uint32]*cacheEntry
}

// NewDetector 创建一个新的 Windows 检测器
func NewDetector() *Detector {
	return &Detector{
		cache: make(map[uint32]*cacheEntry),
	}
}

// FindProcesses 查找所有微信进程并返回它们的信息
func (d *Detector) FindProcesses() ([]*model.Process, error) {
	processes, err := process.Processes()
	if err != nil {
		log.Err(err).Msg("获取进程列表失败")
		return nil, err
	}

	var result []*model.Process
	livePIDs := make(map[uint32]bool)

	for _, p := range processes {
		name, err := p.Name()
		name = strings.TrimSuffix(name, ".exe")
		if err != nil || name != V4ProcessName {
			continue
		}

		pid := uint32(p.Pid)
		livePIDs[pid] = true

		cmdline, cmdlineErr := p.Cmdline()

		procInfo, err := d.getOrProbe(pid, func() (*model.Process, error) {
			return d.getProcessInfo(p)
		})
		if err != nil {
			log.Err(err).Msgf("获取进程 %d 的信息失败", p.Pid)
			continue
		}

		if cmdlineErr == nil && strings.Contains(cmdline, "--") && procInfo.DataDir == "" {
			continue
		}

		result = append(result, procInfo)
	}

	d.pruneStale(livePIDs)
	return result, nil
}

// getOrProbe 对一个 Weixin PID 做探测节流：若缓存已包含 DataDir 非空的结果
// 且未过 ProbeCacheTTL，直接返回缓存；否则调用 probe 刷新。
func (d *Detector) getOrProbe(pid uint32, probe func() (*model.Process, error)) (*model.Process, error) {
	d.mu.Lock()
	if e, ok := d.cache[pid]; ok && e.info != nil && e.info.DataDir != "" && time.Since(e.lastProbed) < ProbeCacheTTL {
		d.mu.Unlock()
		return e.info, nil
	}
	d.mu.Unlock()

	info, err := probe()
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	d.cache[pid] = &cacheEntry{info: info, lastProbed: time.Now()}
	d.mu.Unlock()
	return info, nil
}

// pruneStale 移除 livePIDs 中不存在的缓存项，避免 PID 回收后旧条目残留。
func (d *Detector) pruneStale(livePIDs map[uint32]bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for pid := range d.cache {
		if !livePIDs[pid] {
			delete(d.cache, pid)
		}
	}
}

// getProcessInfo 获取微信进程的详细信息
func (d *Detector) getProcessInfo(p *process.Process) (*model.Process, error) {
	procInfo := &model.Process{
		PID:      uint32(p.Pid),
		Status:   model.StatusOffline,
		Platform: model.PlatformWindows,
	}

	// 获取可执行文件路径
	exePath, err := p.Exe()
	if err != nil {
		log.Err(err).Msg("获取可执行文件路径失败")
		return nil, err
	}
	procInfo.ExePath = exePath

	// 获取版本信息
	versionInfo, err := appver.New(exePath)
	if err != nil {
		log.Err(err).Msg("获取版本信息失败")
		return nil, err
	}
	procInfo.Version = versionInfo.Version
	procInfo.FullVersion = versionInfo.FullVersion

	// 初始化附加信息（数据目录、账户名）
	if err := initializeProcessInfo(p, procInfo); err != nil {
		log.Err(err).Msg("初始化进程信息失败")
		// 即使初始化失败也返回部分信息
	}

	return procInfo, nil
}
