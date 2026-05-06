package wechat

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// makeGenDir 在 generations/{id}/ 下放一个占位文件，并把目录 mtime 拨到 ageAgo 之前
// （模拟"该 generation 已存在 ageAgo 之久"）。
func makeGenDir(t *testing.T, workDir, id string, ageAgo time.Duration) string {
	t.Helper()
	dir := filepath.Join(workDir, "generations", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	// 放一个文件让目录 mtime 变化，再把目录拨回去
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	old := time.Now().Add(-ageAgo)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("chtimes %s: %v", dir, err)
	}
	return dir
}

// listGenIDs 返回 generations/ 下的目录名集合（去掉 .stale 后缀）。
func listGenIDs(t *testing.T, workDir string) []string {
	t.Helper()
	root := filepath.Join(workDir, "generations")
	ents, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("readdir: %v", err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestPruneGenerations_KeepsActive(t *testing.T) {
	workDir := t.TempDir()
	// active gen 即便很老也不能删
	makeGenDir(t, workDir, "20260506-100000", 24*time.Hour)
	// inactive 老 gen 应当被删（120s > 60s grace）
	makeGenDir(t, workDir, "20260506-090000", 120*time.Second)

	res, err := PruneGenerations(PruneOpts{
		WorkDir:     workDir,
		CurrentGen:  "20260506-100000",
		GracePeriod: 60 * time.Second,
		RetryCap:    100 * time.Millisecond,
		RetryDelay:  5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("PruneGenerations: %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "20260506-090000" {
		t.Errorf("expected to remove 20260506-090000, got removed=%v", res.Removed)
	}
	left := listGenIDs(t, workDir)
	if len(left) != 1 || left[0] != "20260506-100000" {
		t.Errorf("expected only active to remain, got %v", left)
	}
}

func TestPruneGenerations_RespectsGrace(t *testing.T) {
	workDir := t.TempDir()
	makeGenDir(t, workDir, "20260506-100000", 24*time.Hour)             // active, 旧但保留
	makeGenDir(t, workDir, "20260506-095959", 10*time.Second)           // inactive 但还在 grace 内

	res, err := PruneGenerations(PruneOpts{
		WorkDir:     workDir,
		CurrentGen:  "20260506-100000",
		GracePeriod: 60 * time.Second,
		RetryCap:    100 * time.Millisecond,
		RetryDelay:  5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("PruneGenerations: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Errorf("expected no removal during grace, got %v", res.Removed)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "20260506-095959" {
		t.Errorf("expected 095959 in Skipped, got %v", res.Skipped)
	}
}

func TestPruneGenerations_NoCurrentSkipsAll(t *testing.T) {
	workDir := t.TempDir()
	makeGenDir(t, workDir, "20260506-090000", 120*time.Second)
	res, err := PruneGenerations(PruneOpts{
		WorkDir:     workDir,
		CurrentGen:  "", // 空 → 保险：什么都别删
		GracePeriod: 60 * time.Second,
		RetryCap:    100 * time.Millisecond,
		RetryDelay:  5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("PruneGenerations: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Errorf("expected fail-safe (no removal) when CurrentGen empty, got %v", res.Removed)
	}
}

func TestPruneGenerations_GenerationsDirMissing(t *testing.T) {
	workDir := t.TempDir()
	// 不创建 generations/，模拟"还没第一次 swap"
	res, err := PruneGenerations(PruneOpts{
		WorkDir:    workDir,
		CurrentGen: "20260506-100000",
	})
	if err != nil {
		t.Fatalf("expected no error when generations/ missing, got %v", err)
	}
	if len(res.Removed)+len(res.Skipped)+len(res.Stale) != 0 {
		t.Errorf("expected empty result, got %+v", res)
	}
}

func TestPruneGenerations_RetriesUntilStale(t *testing.T) {
	workDir := t.TempDir()
	makeGenDir(t, workDir, "20260506-100000", 24*time.Hour)
	makeGenDir(t, workDir, "20260506-090000", 120*time.Second)

	// 注入永不成功的 remove
	failingRemove := func(path string) error {
		return errors.New("ERROR_SHARING_VIOLATION simulated")
	}

	res, err := PruneGenerations(PruneOpts{
		WorkDir:     workDir,
		CurrentGen:  "20260506-100000",
		GracePeriod: 60 * time.Second,
		RetryCap:    20 * time.Millisecond,
		RetryDelay:  5 * time.Millisecond,
		Remove:      failingRemove,
	})
	if err != nil {
		t.Fatalf("PruneGenerations: %v", err)
	}
	if len(res.Stale) != 1 || res.Stale[0] != "20260506-090000" {
		t.Errorf("expected gen marked stale after retry cap, got Stale=%v", res.Stale)
	}
	// .stale marker 文件应当被创建
	stale := filepath.Join(workDir, "generations", "20260506-090000.stale")
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("expected .stale marker at %s, got err=%v", stale, err)
	}
	// 原 gen 目录还在（没删掉）
	gen := filepath.Join(workDir, "generations", "20260506-090000")
	if _, err := os.Stat(gen); err != nil {
		t.Errorf("expected gen dir to still exist when remove fails, got err=%v", err)
	}
}

func TestPruneGenerations_RetriesEventuallySucceeds(t *testing.T) {
	workDir := t.TempDir()
	makeGenDir(t, workDir, "20260506-100000", 24*time.Hour)
	makeGenDir(t, workDir, "20260506-090000", 120*time.Second)

	calls := 0
	flakyRemove := func(path string) error {
		calls++
		if calls < 3 {
			return errors.New("transient sharing violation")
		}
		return os.RemoveAll(path)
	}

	res, err := PruneGenerations(PruneOpts{
		WorkDir:     workDir,
		CurrentGen:  "20260506-100000",
		GracePeriod: 60 * time.Second,
		RetryCap:    100 * time.Millisecond,
		RetryDelay:  2 * time.Millisecond,
		Remove:      flakyRemove,
	})
	if err != nil {
		t.Fatalf("PruneGenerations: %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "20260506-090000" {
		t.Errorf("expected eventual success, got Removed=%v Stale=%v", res.Removed, res.Stale)
	}
	if calls < 3 {
		t.Errorf("expected ≥3 retry attempts, got %d", calls)
	}
}

func TestPruneGenerations_IgnoresStaleMarkerFiles(t *testing.T) {
	workDir := t.TempDir()
	makeGenDir(t, workDir, "20260506-100000", 24*time.Hour)
	// 上次失败留下的 .stale marker（是文件，不是目录）—— prune 不应误把它当 generation
	staleMarker := filepath.Join(workDir, "generations", "20260506-080000.stale")
	if err := os.WriteFile(staleMarker, []byte("failed prune"), 0o600); err != nil {
		t.Fatalf("seed stale marker: %v", err)
	}

	res, err := PruneGenerations(PruneOpts{
		WorkDir:     workDir,
		CurrentGen:  "20260506-100000",
		GracePeriod: 60 * time.Second,
		RetryCap:    100 * time.Millisecond,
		RetryDelay:  5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("PruneGenerations: %v", err)
	}
	if len(res.Removed)+len(res.Stale)+len(res.Skipped) != 0 {
		t.Errorf("expected stale marker ignored (no entries), got %+v", res)
	}
}
