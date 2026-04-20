package wechat

import (
	"testing"
	"time"
)

// publishProgress 的 nil-safe 性 + Phase 读取行为。
// DecryptDBFiles 的端到端进度（需要真 encrypted db fixture）不在测试 scope 里，
// 由集成测试 / 人工验证覆盖。

func TestPublishProgress_NilPublisher_NoOp(t *testing.T) {
	svc := NewService(&mockConfig{})
	// 手动清空 publisher 模拟 NewService 失败场景
	svc.mutex.Lock()
	svc.progressPub = nil
	svc.mutex.Unlock()

	// 不应 panic
	svc.publishProgress(1, 10, 100, 1000, "test.db", time.Now())
}

func TestPublishProgress_SetsCurrentPhase(t *testing.T) {
	svc := NewService(&mockConfig{})
	svc.SetPhase(PhaseFirstFull)

	ch, cancel := svc.Subscribe()
	defer cancel()

	svc.publishProgress(3, 10, 300, 1000, "msg_0.db", time.Now())

	select {
	case evt := <-ch:
		if evt.Phase != PhaseFirstFull {
			t.Errorf("Phase = %q, want first_full", evt.Phase)
		}
		if evt.FilesDone != 3 || evt.FilesTotal != 10 {
			t.Errorf("Files = %d/%d, want 3/10", evt.FilesDone, evt.FilesTotal)
		}
		if evt.BytesDone != 300 || evt.BytesTotal != 1000 {
			t.Errorf("Bytes = %d/%d, want 300/1000", evt.BytesDone, evt.BytesTotal)
		}
		if evt.CurrentFile != "msg_0.db" {
			t.Errorf("CurrentFile = %q, want msg_0.db", evt.CurrentFile)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected progress event")
	}
}

func TestServiceSubscribe_AfterStop_RefreshedOnStart(t *testing.T) {
	svc := NewService(&mockConfig{})

	// 第一次 Subscribe
	ch1, cancel1 := svc.Subscribe()
	defer cancel1()

	svc.SetPhase(PhaseFirstFull)
	svc.publishProgress(1, 10, 100, 1000, "f1.db", time.Now())
	select {
	case <-ch1:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("first subscriber should receive first event")
	}

	// 模拟 Stop 流程：close publisher
	_ = svc.StopAutoDecrypt()

	// ch1 应被 close
	select {
	case _, ok := <-ch1:
		if ok {
			// 可能是之前的 buffered event 还没 drain，再读一次
			select {
			case _, ok2 := <-ch1:
				if ok2 {
					t.Error("ch1 should be closed after Stop")
				}
			case <-time.After(50 * time.Millisecond):
				t.Error("ch1 should be closed, got hang")
			}
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("ch1 should be closed after Stop")
	}

	// StartAutoDecrypt 会重建 publisher —— 这里直接模拟 refresh
	svc.mutex.Lock()
	if svc.progressPub == nil {
		svc.progressPub = NewProgressPublisher()
	}
	svc.mutex.Unlock()

	ch2, cancel2 := svc.Subscribe()
	defer cancel2()

	svc.publishProgress(2, 10, 200, 1000, "f2.db", time.Now())
	select {
	case evt := <-ch2:
		if evt.FilesDone != 2 {
			t.Errorf("ch2 should get event 2, got %d", evt.FilesDone)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ch2 should receive after refresh")
	}
}
