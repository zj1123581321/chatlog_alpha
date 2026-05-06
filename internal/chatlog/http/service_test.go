package http

import (
	"net/http"
	"testing"
	"time"
)

// TestNewHTTPServer_HasReadTimeout 验证 newHTTPServer 设置了 ReadTimeout。
// 没有 ReadTimeout 时，半死客户端 (TCP 已建立但不发请求) 会无限占用 server 资源，
// 长跑下导致 handle/goroutine 累积。日志已观察到 530s elapsed 的 slow request。
func TestNewHTTPServer_HasReadTimeout(t *testing.T) {
	srv := newHTTPServer("127.0.0.1:0", http.DefaultServeMux)

	if srv.ReadTimeout == 0 {
		t.Errorf("ReadTimeout must be set, got 0 (means infinite, allows half-dead connections)")
	}
	if srv.ReadTimeout > 60*time.Second {
		t.Errorf("ReadTimeout too lax: %v, half-dead clients can hold connection too long", srv.ReadTimeout)
	}
}

// TestNewHTTPServer_HasWriteTimeout 验证 WriteTimeout。
// 客户端断开但 server 还在写时，没有 WriteTimeout 会让 goroutine 无限挂起。
func TestNewHTTPServer_HasWriteTimeout(t *testing.T) {
	srv := newHTTPServer("127.0.0.1:0", http.DefaultServeMux)

	if srv.WriteTimeout == 0 {
		t.Errorf("WriteTimeout must be set, got 0 (means infinite)")
	}
	if srv.WriteTimeout > 120*time.Second {
		t.Errorf("WriteTimeout too lax: %v", srv.WriteTimeout)
	}
}

// TestNewHTTPServer_HasIdleTimeout 验证 IdleTimeout。
// 长连接 keep-alive 闲置不释放会持续占用 handle。
func TestNewHTTPServer_HasIdleTimeout(t *testing.T) {
	srv := newHTTPServer("127.0.0.1:0", http.DefaultServeMux)

	if srv.IdleTimeout == 0 {
		t.Errorf("IdleTimeout must be set, idle keep-alive connections accumulate handles")
	}
}

// TestNewHTTPServer_HandlerWired 确保 handler 真的注入了，不是 nil。
func TestNewHTTPServer_HandlerWired(t *testing.T) {
	mux := http.NewServeMux()
	srv := newHTTPServer("127.0.0.1:0", mux)

	if srv.Handler == nil {
		t.Fatal("Handler must be wired through")
	}
}

// TestNewHTTPServer_AddrWired 确保 addr 也是真的注入。
func TestNewHTTPServer_AddrWired(t *testing.T) {
	srv := newHTTPServer("0.0.0.0:5030", http.DefaultServeMux)

	if srv.Addr != "0.0.0.0:5030" {
		t.Errorf("Addr=%q want 0.0.0.0:5030", srv.Addr)
	}
}
