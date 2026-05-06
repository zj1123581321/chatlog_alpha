package wechat

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestWatcherDaemon_RunOnce_HappyPath：
// 一次 RunOnce 应该完成一次 cycle + tick watchdog + 期望次数的 prune。
func TestWatcherDaemon_RunOnce_HappyPath(t *testing.T) {
	workDir := t.TempDir()
	dataDir := t.TempDir()
	rels := []string{"message/multi/message_0.db"}
	fakeDataDir(t, dataDir, rels)

	wd := &Watchdog{
		PhaseFn: func() AutoDecryptPhase { return PhaseLive },
		Now:     time.Now,
		Exit:    func(int) { t.Fatalf("exit must not be called in happy path") },
	}

	d := &WatcherDaemon{
		Opts: WatcherDaemonOpts{
			WorkDir: workDir,
			DataDir: dataDir,
			DBs:     []DBJob{msgJob(rels[0])},
			DecryptFunc: fakeDecryptWithSchema(rels,
				[]string{`CREATE TABLE Timestamp (ts INTEGER)`}, nil),
			PruneEvery: 1, // 每个 cycle 都跑 prune
			WatcherPID: 9999,
		},
		Watchdog: wd,
	}

	res, err := d.RunOnce()
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if res.Outcome != OutcomeSwapped {
		t.Errorf("expected Swapped, got %s reason=%s", res.Outcome, res.Reason)
	}
	// status.json 写盘 + heartbeat 更新
	st, err := ReadStatus(workDir)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if st.WatcherPID != 9999 {
		t.Errorf("WatcherPID = %d, want 9999", st.WatcherPID)
	}
	// watchdog 已 tick：lastTickNs 非零
	if atomic.LoadInt64(&wd.lastTickNs) == 0 {
		t.Errorf("expected watchdog to be ticked")
	}
}

// TestWatcherDaemon_RunOnce_PrunesAfterMultipleCycles：
// 第一次 cycle 后旧 generation 还在；第二次 cycle 应当 prune 掉第一次的（grace 用极小值）。
func TestWatcherDaemon_RunOnce_PrunesAfterMultipleCycles(t *testing.T) {
	workDir := t.TempDir()
	dataDir := t.TempDir()
	rels := []string{"message/multi/message_0.db"}
	fakeDataDir(t, dataDir, rels)

	d := &WatcherDaemon{
		Opts: WatcherDaemonOpts{
			WorkDir: workDir,
			DataDir: dataDir,
			DBs:     []DBJob{msgJob(rels[0])},
			DecryptFunc: fakeDecryptWithSchema(rels,
				[]string{`CREATE TABLE Timestamp (ts INTEGER)`}, nil),
			PruneEvery:        1,
			PruneGracePeriod:  1 * time.Millisecond, // 极小 grace
			PruneRetryDelay:   1 * time.Millisecond,
			PruneRetryCap:     50 * time.Millisecond,
			WatcherPID:        9999,
		},
	}

	first, err := d.RunOnce()
	if err != nil || first.Outcome != OutcomeSwapped {
		t.Fatalf("first cycle failed: %+v err=%v", first, err)
	}
	// 等 grace
	time.Sleep(20 * time.Millisecond)

	second, err := d.RunOnce()
	if err != nil || second.Outcome != OutcomeSwapped {
		t.Fatalf("second cycle failed: %+v err=%v", second, err)
	}
	if first.GenerationID == second.GenerationID {
		t.Errorf("expected distinct gen ids")
	}

	// 第一代 generation 应已被 prune 掉
	if _, err := os.Stat(ResolveGenerationDir(workDir, first.GenerationID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected first generation to be pruned, got stat err=%v", err)
	}
	// 第二代仍在
	if _, err := os.Stat(ResolveGenerationDir(workDir, second.GenerationID)); err != nil {
		t.Errorf("expected second generation to remain: %v", err)
	}
}

// TestWatcherDaemon_RunOnce_WatchdogNotTickedOnFailedCycle：
// cycle 失败时 watchdog 仍应 tick（防止假 hang），但 outcome=Corrupt。
func TestWatcherDaemon_RunOnce_WatchdogTickedEvenOnCorrupt(t *testing.T) {
	workDir := t.TempDir()
	dataDir := t.TempDir()
	rels := []string{"missing.db"}
	// 故意不 fakeDataDir → copy 必败

	wd := &Watchdog{
		PhaseFn: func() AutoDecryptPhase { return PhaseLive },
		Now:     time.Now,
		Exit:    func(int) {},
	}
	d := &WatcherDaemon{
		Opts: WatcherDaemonOpts{
			WorkDir:     workDir,
			DataDir:     dataDir,
			DBs:         []DBJob{msgJob(rels[0])},
			DecryptFunc: func(rawDir, dstDir string) error { return nil },
			PruneEvery:  1,
		},
		Watchdog: wd,
	}

	res, _ := d.RunOnce()
	if res.Outcome != OutcomeCorrupt {
		t.Errorf("expected corrupt, got %s", res.Outcome)
	}
	if atomic.LoadInt64(&wd.lastTickNs) == 0 {
		t.Errorf("watchdog should be ticked even on corrupt")
	}
}

// TestWatcherDaemon_RunOnce_PruneEveryGate：PruneEvery=3 时只在第 3/6/... 次跑 prune。
func TestWatcherDaemon_RunOnce_PruneEveryGate(t *testing.T) {
	workDir := t.TempDir()
	dataDir := t.TempDir()
	rels := []string{"message/multi/message_0.db"}
	fakeDataDir(t, dataDir, rels)

	d := &WatcherDaemon{
		Opts: WatcherDaemonOpts{
			WorkDir: workDir,
			DataDir: dataDir,
			DBs:     []DBJob{msgJob(rels[0])},
			DecryptFunc: fakeDecryptWithSchema(rels,
				[]string{`CREATE TABLE Timestamp (ts INTEGER)`}, nil),
			PruneEvery:       3,
			PruneGracePeriod: 1 * time.Millisecond,
		},
	}

	// 跑 2 次 → 还没到 prune 时机
	for i := 0; i < 2; i++ {
		if _, err := d.RunOnce(); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 此刻 generations/ 下应有 2 个目录
	ents, _ := os.ReadDir(filepath.Join(workDir, "generations"))
	if len(ents) != 2 {
		t.Errorf("after 2 cycles expected 2 gens (no prune yet), got %d: %v", len(ents), namesOf(ents))
	}
}

// TestWatcherDaemon_DecryptFuncRequired：未设 DecryptFunc 时 RunOnce 必报错（构造期 fail-fast）。
func TestWatcherDaemon_DecryptFuncRequired(t *testing.T) {
	d := &WatcherDaemon{
		Opts: WatcherDaemonOpts{
			WorkDir:    t.TempDir(),
			DataDir:    t.TempDir(),
			DBs:        []DBJob{msgJob("x.db")},
			PruneEvery: 1,
			// DecryptFunc nil
		},
	}
	if _, err := d.RunOnce(); err == nil {
		t.Errorf("expected error when DecryptFunc nil")
	}
}
