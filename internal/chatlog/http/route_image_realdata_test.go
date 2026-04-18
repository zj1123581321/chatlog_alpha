//go:build realdata
// +build realdata

package http

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestServeImage_UserExampleE2E 用真实的用户环境做端到端验证:
//   - 真实 backup 目录 D:\WXAutoImage (含 277 个群目录)
//   - 真实配置 backup_folder_map: 27580424670@chatroom → C606ACFA
//   - 真实示例图片 拼车群(C606ACFA)/2026-04/202604171741320215_微信图片(Zzz).jpg
//   - 模拟 /api/v1/chatlog 预热后的 md5PathCache 状态
//
// 运行方式:
//   go test -tags=realdata -run=TestServeImage_UserExampleE2E -v ./internal/chatlog/http/
//
// 预期:
//   - HTTP 200
//   - 响应头 X-Image-Source: backup
//   - 响应 body = 真实 jpg 文件内容 (不是缩略图, 不是 dat2img 失败的 500 JSON)
//   - 字节数 > 1KB (排除"一个空返回也算通过")
//   - stats.backup = 1
func TestServeImage_UserExampleE2E(t *testing.T) {
	const (
		backupRoot  = `D:\WXAutoImage`
		md5         = "291e0478709e39c92b56e0236d73cd19"
		talker      = "27580424670@chatroom"
		expectFile  = `D:\WXAutoImage\拼车群(C606ACFA)\2026-04\202604171741320215_微信图片(Zzz).jpg`
	)

	// 用户给的例子: 2026-04-17T17:41:32+08:00
	msgTime := time.Date(2026, 4, 17, 17, 41, 32, 0, time.Local)

	// Sanity check: backup 目录和目标图片必须真实存在
	if _, err := os.Stat(backupRoot); err != nil {
		t.Skipf("backup root not accessible, skipping real-data test: %v", err)
	}
	realBytes, err := os.ReadFile(expectFile)
	if err != nil {
		t.Skipf("expected backup file not found, skipping: %v", err)
	}
	t.Logf("real backup image: %d bytes at %s", len(realBytes), expectFile)

	// 构造 Service: 不连 DB (db=nil), 只接通 backup 路径
	gin.SetMode(gin.TestMode)
	router := gin.New()
	s := &Service{
		conf: &mockImageConfig{
			dataDir:         t.TempDir(), // 空 data dir, 走不到 hardlink/cache/recurse
			backupPath:      backupRoot,
			backupFolderMap: map[string]string{talker: "C606ACFA"},
		},
		router:       router,
		md5PathCache: make(map[string]CachedMediaMeta),
		backupIndex: NewBackupIndex(backupRoot, map[string]string{
			talker: "C606ACFA",
		}),
	}
	if err := s.backupIndex.Scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	chat, hex, unk := s.backupIndex.Stats()
	t.Logf("backup index: chatroom=%d hex=%d unknown=%d", chat, hex, unk)
	if hex == 0 {
		t.Fatal("expected hex_mode dirs > 0, got 0 (backup_path maybe wrong?)")
	}
	s.initMediaRouter()

	// 模拟 /api/v1/chatlog 预热后 cache 状态
	s.md5PathCache[md5] = CachedMediaMeta{
		Path:   filepath.Join("msg", "attach", "xxx", "2026-04", "Img", md5),
		Talker: talker,
		Time:   msgTime,
	}

	// 验证 resolve
	dir, via, ok := s.backupIndex.Resolve(talker)
	if !ok {
		t.Fatalf("talker %s did not resolve via backupIndex", talker)
	}
	t.Logf("resolved: talker=%s → dir=%s (via=%s)", talker, dir, via)

	// 发请求
	req := httptest.NewRequest(http.MethodGet, "/image/"+md5, nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	// 断言
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Image-Source"); got != "backup" {
		t.Errorf("expected X-Image-Source=backup, got %q", got)
	}
	if got := s.backupStats.Backup.Load(); got != 1 {
		t.Errorf("expected stats.backup=1, got %d; all=%v", got, s.backupStats.snapshot())
	}

	gotBytes := w.Body.Bytes()
	t.Logf("response: %d bytes, Content-Type=%s", len(gotBytes), w.Header().Get("Content-Type"))

	if len(gotBytes) < 1024 {
		t.Fatalf("response too small (%d bytes), not a real image", len(gotBytes))
	}
	if !bytes.Equal(gotBytes, realBytes) {
		t.Errorf("response body != disk file (got %d bytes vs %d on disk)", len(gotBytes), len(realBytes))
	}
	// JPEG magic bytes: FF D8 FF
	if len(gotBytes) >= 3 && (gotBytes[0] != 0xFF || gotBytes[1] != 0xD8 || gotBytes[2] != 0xFF) {
		t.Errorf("response does not start with JPEG magic bytes: % x", gotBytes[:3])
	}
}
