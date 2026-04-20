package wechat

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func makeEvt(done, total int) ProgressEvent {
	return ProgressEvent{
		Phase:      PhaseFirstFull,
		FilesDone:  done,
		FilesTotal: total,
		UpdatedAt:  time.Now(),
	}
}

func TestProgressPublisher_Subscribe_CapIsOne(t *testing.T) {
	pub := NewProgressPublisher()
	ch, cancel := pub.Subscribe()
	defer cancel()

	// 无消费者时第一次 publish 应立即入 chan
	pub.Publish(makeEvt(1, 10))

	select {
	case evt := <-ch:
		if evt.FilesDone != 1 {
			t.Errorf("got FilesDone=%d, want 1", evt.FilesDone)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected first event within 100ms")
	}
}

func TestProgressPublisher_SlowConsumer_DropsOldKeepsNewest(t *testing.T) {
	pub := NewProgressPublisher()
	ch, cancel := pub.Subscribe()
	defer cancel()

	// 连续发 10 条，中间不消费
	for i := 0; i < 10; i++ {
		pub.Publish(makeEvt(i+1, 10))
	}

	// 第一条 drain: 应该是最新那条（FilesDone=10）
	select {
	case evt := <-ch:
		if evt.FilesDone != 10 {
			t.Errorf("keep-latest: got FilesDone=%d, want 10 (latest)", evt.FilesDone)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected at least one event")
	}

	// 下一个 drain 应该没有（slow consumer 丢了中间的 1-9）
	select {
	case evt := <-ch:
		t.Errorf("should not have more events, got FilesDone=%d", evt.FilesDone)
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestProgressPublisher_MultipleSubscribers_EachGetsEvents(t *testing.T) {
	pub := NewProgressPublisher()

	ch1, cancel1 := pub.Subscribe()
	ch2, cancel2 := pub.Subscribe()
	defer cancel1()
	defer cancel2()

	pub.Publish(makeEvt(5, 10))

	for i, ch := range []<-chan ProgressEvent{ch1, ch2} {
		select {
		case evt := <-ch:
			if evt.FilesDone != 5 {
				t.Errorf("subscriber %d: got FilesDone=%d, want 5", i, evt.FilesDone)
			}
		case <-time.After(100 * time.Millisecond):
			t.Errorf("subscriber %d: no event received", i)
		}
	}
}

func TestProgressPublisher_Close_UnblocksSubscribers(t *testing.T) {
	pub := NewProgressPublisher()
	ch, _ := pub.Subscribe()

	// goroutine range on ch
	done := make(chan struct{})
	var received int32
	go func() {
		defer close(done)
		for range ch {
			atomic.AddInt32(&received, 1)
		}
	}()

	pub.Publish(makeEvt(1, 10))
	pub.Publish(makeEvt(2, 10))
	time.Sleep(20 * time.Millisecond) // let consumer catch up

	pub.Close()

	select {
	case <-done:
		// range exited cleanly
	case <-time.After(500 * time.Millisecond):
		t.Fatal("range should exit after Close")
	}
}

func TestProgressPublisher_CancelSubscription_RemovesFromList(t *testing.T) {
	pub := NewProgressPublisher()
	ch1, cancel1 := pub.Subscribe()
	ch2, cancel2 := pub.Subscribe()
	defer cancel2()

	// 取消 ch1
	cancel1()

	// ch1 应该 closed
	select {
	case _, ok := <-ch1:
		if ok {
			t.Error("ch1 should be closed after cancel, got value")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("ch1 should be closed after cancel")
	}

	// ch2 应该正常收事件
	pub.Publish(makeEvt(3, 10))
	select {
	case evt := <-ch2:
		if evt.FilesDone != 3 {
			t.Errorf("ch2 should still receive, got FilesDone=%d", evt.FilesDone)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("ch2 should still receive events")
	}

	// cancel 应幂等
	cancel1() // 不应 panic
}

func TestProgressPublisher_PublishAfterClose_NoOp(t *testing.T) {
	pub := NewProgressPublisher()
	pub.Close()
	// 应不 panic
	pub.Publish(makeEvt(1, 10))
}

// --- progressLogThrottle 节流单测 ---

func TestProgressLogThrottle_FirstEvent_AlwaysLogs(t *testing.T) {
	evt := ProgressEvent{FilesDone: 1, FilesTotal: 10, BytesDone: 100, BytesTotal: 1000}
	if !progressLogThrottle(evt, -1.0, time.Time{}) {
		t.Error("first event (lastLogPct == -1) should log")
	}
}

func TestProgressLogThrottle_FinalEvent_AlwaysLogs(t *testing.T) {
	evt := ProgressEvent{FilesDone: 10, FilesTotal: 10, BytesDone: 1000, BytesTotal: 1000}
	if !progressLogThrottle(evt, 99.0, time.Now()) {
		t.Error("final event (FilesDone == FilesTotal) should log")
	}
}

func TestProgressLogThrottle_Below5Pct_NoLog(t *testing.T) {
	// 已打过 10%，现在 11%，差 1% < 5%，不应打
	evt := ProgressEvent{FilesDone: 11, FilesTotal: 100, BytesDone: 110, BytesTotal: 1000}
	if progressLogThrottle(evt, 10.0, time.Now()) {
		t.Error("delta 1%% (< 5%%) should NOT log")
	}
}

func TestProgressLogThrottle_Above5Pct_Logs(t *testing.T) {
	// 已打过 10%，现在 15.5%，差 5.5% >= 5%，应打
	evt := ProgressEvent{FilesDone: 15, FilesTotal: 100, BytesDone: 155, BytesTotal: 1000}
	if !progressLogThrottle(evt, 10.0, time.Now()) {
		t.Error("delta >= 5%% should log")
	}
}

func TestProgressLogThrottle_TimeBased_LogsAfter30s(t *testing.T) {
	// 进度只涨了 1%，但上次打印是 31 秒前，应打
	evt := ProgressEvent{FilesDone: 11, FilesTotal: 100, BytesDone: 110, BytesTotal: 1000}
	if !progressLogThrottle(evt, 10.0, time.Now().Add(-31*time.Second)) {
		t.Error("31s since last log should trigger regardless of pct")
	}
}

func TestProgressLogThrottle_ZeroTotalBytes_DoesNotLog(t *testing.T) {
	// BytesTotal=0 且已过首次：不应 log，避免 div-by-zero
	evt := ProgressEvent{FilesDone: 5, FilesTotal: 10, BytesDone: 0, BytesTotal: 0}
	if progressLogThrottle(evt, 50.0, time.Now()) {
		t.Error("zero total bytes should not log (except first / final)")
	}
}

func TestProgressPublisher_Concurrent_PublishAndSubscribe_RaceClean(t *testing.T) {
	// go test -race 下跑：验证并发 publish / subscribe / cancel 不爆 race detector
	pub := NewProgressPublisher()
	defer pub.Close()

	var wg sync.WaitGroup

	// 5 个 publisher
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				pub.Publish(makeEvt(idx*100+j, 500))
			}
		}(i)
	}

	// 5 个 subscriber 动态进出
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancel := pub.Subscribe()
			defer cancel()
			// 消费一会儿
			deadline := time.After(50 * time.Millisecond)
			for {
				select {
				case <-ch:
				case <-deadline:
					return
				}
			}
		}()
	}

	wg.Wait()
}
