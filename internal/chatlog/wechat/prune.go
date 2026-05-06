package wechat

// prune.go：Step 5f Generation prune（architecture-rework-2026-05-06.md
// Eng Review Lock A3 watcher 端 prune）。
//
// 安全规则：
//   - 永不动 active generation（CurrentGen 指定的那个）。
//   - inactive generation 的目录 mtime 距今 ≥ GracePeriod 才会被删，
//     给 server 时间完成 invalidate + 在途 query 用 fd 自然结束。
//   - os.RemoveAll 在 Windows 上会被 ERROR_SHARING_VIOLATION（server 还持有 fd
//     的瞬间）打回。我们 sleep RetryDelay 后重试，累计上限 RetryCap。
//   - 仍失败 → 落 .stale marker 让下个 prune cycle 再试，不强删（避免破坏 server）。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 默认值与 spec §A3 对齐：60s grace、5s retry delay、5min retry cap。
const (
	DefaultPruneGracePeriod = 60 * time.Second
	DefaultPruneRetryDelay  = 5 * time.Second
	DefaultPruneRetryCap    = 5 * time.Minute
)

// PruneOpts 是 PruneGenerations 的输入。除 Remove 外都是值类型（可零值，按 default 填）。
type PruneOpts struct {
	// WorkDir 是 chatlog 工作目录；prune 操作 WorkDir/generations/ 子树。
	WorkDir string

	// CurrentGen 是当前 active 的 generation id（status.json.current_generation）。
	// 空字符串 → fail-safe，本次 prune 不删任何东西（避免在状态未初始化时误删）。
	CurrentGen string

	// GracePeriod 距今多久才考虑删除 inactive generation。
	// 0 → DefaultPruneGracePeriod。
	GracePeriod time.Duration

	// RetryDelay 单次 ERROR_SHARING_VIOLATION 后等多久重试。0 → DefaultPruneRetryDelay。
	RetryDelay time.Duration

	// RetryCap 单个 generation 的总 retry 时间上限。0 → DefaultPruneRetryCap。
	// 超过这个上限仍删不掉 → 落 .stale marker，本轮放弃，下轮再试。
	RetryCap time.Duration

	// Remove 是注入点，默认 os.RemoveAll。生产无需设；测试用来模拟
	// ERROR_SHARING_VIOLATION 等失败模式。
	Remove func(path string) error

	// Now 注入点，默认 time.Now。测试用来固定时间。
	Now func() time.Time
}

// PruneResult 总结一次 PruneGenerations 的处置结果。所有 slice 是 generation id（短字符串）。
type PruneResult struct {
	// Removed: 已成功删除的 inactive generation。
	Removed []string
	// Skipped: 在 grace 期内或是 active，本次跳过。
	Skipped []string
	// Stale: 重试到 cap 仍删不掉，已落 .stale marker 等下轮。
	Stale []string
}

// PruneGenerations 扫 WorkDir/generations/ 下所有子目录，按规则清理。
//
// 不会返回部分失败错误：单个 generation 删不掉就标 .stale 并继续处理下一个，
// 让 prune 一轮调用尽可能多地推进进度。返回的 error 仅用于"完全没法读 generations/"
// 这种致命情况。
func PruneGenerations(opts PruneOpts) (PruneResult, error) {
	if opts.GracePeriod <= 0 {
		opts.GracePeriod = DefaultPruneGracePeriod
	}
	if opts.RetryDelay <= 0 {
		opts.RetryDelay = DefaultPruneRetryDelay
	}
	if opts.RetryCap <= 0 {
		opts.RetryCap = DefaultPruneRetryCap
	}
	if opts.Remove == nil {
		opts.Remove = os.RemoveAll
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	root := filepath.Join(opts.WorkDir, "generations")
	ents, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PruneResult{}, nil
		}
		return PruneResult{}, fmt.Errorf("prune: readdir %s: %w", root, err)
	}

	var res PruneResult
	now := opts.Now()
	for _, ent := range ents {
		if !ent.IsDir() {
			// 忽略 .stale marker 文件、临时文件、操作系统垃圾。
			continue
		}
		id := ent.Name()
		if strings.HasSuffix(id, ".stale") {
			// 防御：理论上 .stale 是文件不是目录，但有人手工弄成目录也兜住。
			continue
		}

		// active 永远不动；CurrentGen 空时整个 prune 退化为 no-op（fail-safe）。
		// 不进 Skipped —— Skipped 专指"本可删但 grace 内推迟"的事件，active 是基线。
		if id == opts.CurrentGen {
			continue
		}
		if opts.CurrentGen == "" {
			continue
		}

		genPath := filepath.Join(root, id)
		stat, err := os.Stat(genPath)
		if err != nil {
			res.Skipped = append(res.Skipped, id)
			continue
		}
		age := now.Sub(stat.ModTime())
		if age < opts.GracePeriod {
			res.Skipped = append(res.Skipped, id)
			continue
		}

		// 走 retry 循环
		removed, err := removeWithRetry(genPath, opts.Remove, opts.RetryDelay, opts.RetryCap)
		if err == nil && removed {
			res.Removed = append(res.Removed, id)
			continue
		}
		// retry cap 内仍失败：落 .stale marker
		stalePath := filepath.Join(root, id+".stale")
		_ = os.WriteFile(stalePath, []byte(fmt.Sprintf("prune cap exceeded: %v\n", err)), 0o600)
		res.Stale = append(res.Stale, id)
	}
	return res, nil
}

// removeWithRetry 反复 remove 直到成功或累计耗时超 cap。
// 返回 (removed=true, err=nil) 表示成功；(false, lastErr) 表示放弃。
func removeWithRetry(path string, remove func(string) error, delay, cap time.Duration) (bool, error) {
	deadline := time.Now().Add(cap)
	var lastErr error
	for {
		err := remove(path)
		if err == nil {
			return true, nil
		}
		lastErr = err
		if time.Now().Add(delay).After(deadline) {
			return false, lastErr
		}
		time.Sleep(delay)
	}
}
