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

	// EmptyInfoProbeCacheTTL 对"尚未探测出 DataDir"的 PID 做节流。
	// 主进程的 session.db 并非一直被持有（Weixin 4.x 观察到稳态只有
	// message_fts.db / favorite_*.db-wal 等被保持打开），只盯 session.db
	// 会把这些主进程当成未登录一直重探。Weixin 还会拉多个 renderer 子进程，
	// 它们的 OpenFiles 永远没有 db 路径，再也不会命中。
	// 不给这类 PID 缓存就会每秒 p.OpenFiles 一次，每次漏 1 个 Process HANDLE
	// （gopsutil v4.25.7 OpenFilesWithContext 漏关 OpenProcess 返回的 handle）。
	// 这里给 30 秒兜底，既抑制泄漏又保留 "用户刚启动 chatlog 后新登录微信"
	// 的可感知延迟。
	//
	// DataDir 一旦识别出来 (非空) 就永久缓存, 不受任何 TTL 影响 ——
	// PID 进程生命周期内 DataDir 是不变量, 重探只会触发 gopsutil HANDLE 泄漏
	// 而拿不到任何新信息. 这就是 Step 2 (detector startup-only) 的核心:
	// OpenFiles 的真实调用频率从 "每秒" 降到 "每个 PID 一生一次".
	EmptyInfoProbeCacheTTL = 30 * time.Second
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

		// Weixin 会起多个 renderer/gpu 子进程，它们 cmdline 带 `--` 且从不开 db_storage
		// 文件。对它们调 p.OpenFiles 只会命中 gopsutil 的 HANDLE 泄漏 bug 而拿不到
		// 有用信息，直接 continue，连缓存条目都不留。
		if cmdline, err := p.Cmdline(); err == nil && strings.Contains(cmdline, "--") {
			continue
		}

		procInfo, err := d.getOrProbe(pid, func() (*model.Process, error) {
			return d.getProcessInfo(p)
		})
		if err != nil {
			log.Err(err).Msgf("获取进程 %d 的信息失败", p.Pid)
			continue
		}

		result = append(result, procInfo)
	}

	d.pruneStale(livePIDs)
	return result, nil
}

// getOrProbe 对一个 Weixin PID 做探测节流。
// 命中任一分支就返回缓存、不调 probe：
//   - DataDir 已识别出来 (非空) → 永久缓存 (除非 PID 被 prune)，DataDir 在进程
//     生命周期内是不变量；
//   - DataDir 空 → 短 TTL（EmptyInfoProbeCacheTTL），兜底抑制 gopsutil p.OpenFiles
//     的 HANDLE 泄漏，同时保留微信登录后在合理时间内被识别的响应性。
func (d *Detector) getOrProbe(pid uint32, probe func() (*model.Process, error)) (*model.Process, error) {
	d.mu.Lock()
	if e, ok := d.cache[pid]; ok && e.info != nil {
		// DataDir 已识别 → 永久 cache，绝不重探
		if e.info.DataDir != "" {
			d.mu.Unlock()
			return e.info, nil
		}
		// DataDir 空 → 短 TTL 内复用，超时 reprobe
		if time.Since(e.lastProbed) < EmptyInfoProbeCacheTTL {
			d.mu.Unlock()
			return e.info, nil
		}
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
