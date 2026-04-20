package http

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mark3labs/mcp-go/server"
	"github.com/rs/zerolog/log"

	"github.com/sjzar/chatlog/internal/chatlog/database"
	"github.com/sjzar/chatlog/internal/errors"
)

// CachedMediaMeta 是 /api/v1/chatlog 预热填充的媒体元数据, 供 /image/{md5} 在
// hardlink 未命中或只命中缩略图时用于进一步兜底查询 (例如 backup 文件夹)。
//
// Talker 和 Time 在没有前置 /chatlog 调用时可能为零值: 该情况下 backup 兜底
// 无法启用, 最终只能回退到缩略图。
type CachedMediaMeta struct {
	Path   string
	Talker string
	Time   time.Time
}

type Service struct {
	conf Config
	db   *database.Service

	router *gin.Engine
	server *http.Server

	mcpServer           *server.MCPServer
	mcpSSEServer        *server.SSEServer
	mcpStreamableServer *server.StreamableHTTPServer

	// md5 到媒体元数据的缓存, 由 /api/v1/chatlog 调用填充
	md5PathCache map[string]CachedMediaMeta
	md5PathMu    sync.RWMutex

	// backup 目录反查索引, 启动时 Scan 一次, /api/v1/cache/clear 可触发重建
	backupIndex *BackupIndex

	// backup 请求来源分布统计 (原子计数), 暴露给 /api/v1/backup/stats
	backupStats backupStats

	// autoDecryptPhaseFn 返回当前自动解密 phase 字符串（"idle" / "first_full" / 等）。
	// 由 manager 启动后 SetAutoDecryptPhaseFunc 注入。nil 时 gate middleware 直通。
	//
	// 作用：Codex outside voice Tension #1 "输出一致性契约" 的解法 ——
	// 首次全量（phase=="first_full"）期间 /api/v1/chatlog 等读数据接口返 503，
	// 避免 HTTP 消费者看到跨 db 新旧混合的时间线断层。
	autoDecryptPhaseFn func() string

	// autoDecryptStatusFn 返回完整 status snapshot，供 /api/v1/autodecrypt/status
	// 消费。phase / enabled / last_run 都在里面。
	autoDecryptStatusFn AutoDecryptStatusGetter
}

// SetAutoDecryptPhaseFunc 注入 phase 查询闭包，用于 503 gate middleware。
// 幂等：多次调用覆盖前值。传 nil 禁用 gate（测试场景）。
func (s *Service) SetAutoDecryptPhaseFunc(fn func() string) {
	s.autoDecryptPhaseFn = fn
}

type Config interface {
	GetHTTPAddr() string
	GetDataDir() string
	GetSaveDecryptedMedia() bool
	GetBackupPath() string
	GetBackupFolderMap() map[string]string
}

func NewService(conf Config, db *database.Service) *Service {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()

	// Handle error from SetTrustedProxies
	if err := router.SetTrustedProxies(nil); err != nil {
		log.Err(err).Msg("Failed to set trusted proxies")
	}

	// Middleware
	router.Use(
		errors.RecoveryMiddleware(),
		errors.ErrorHandlerMiddleware(),
		gin.LoggerWithWriter(log.Logger, "/health"),
		corsMiddleware(),
		slowRequestMiddleware(SlowRequestThreshold),
	)

	s := &Service{
		conf:         conf,
		db:           db,
		router:       router,
		md5PathCache: make(map[string]CachedMediaMeta),
		backupIndex:  NewBackupIndex(conf.GetBackupPath(), conf.GetBackupFolderMap()),
	}

	// 启动时同步扫一次 backup 目录。失败不阻塞启动 (Scan 内部只 Warn)。
	_ = s.backupIndex.Scan()

	s.initMCPServer()
	s.initRouter()
	return s
}

func (s *Service) Start() error {

	s.server = &http.Server{
		Addr:    s.conf.GetHTTPAddr(),
		Handler: s.router,
	}

	go func() {
		// Handle error from Run
		if err := s.server.ListenAndServe(); err != nil {
			log.Err(err).Msg("Failed to start HTTP server")
		}
	}()

	log.Info().Msg("Starting HTTP server on " + s.conf.GetHTTPAddr())

	return nil
}

func (s *Service) ListenAndServe() error {

	s.server = &http.Server{
		Addr:    s.conf.GetHTTPAddr(),
		Handler: s.router,
	}

	log.Info().Msg("Starting HTTP server on " + s.conf.GetHTTPAddr())
	return s.server.ListenAndServe()
}

func (s *Service) Stop() error {

	if s.server == nil {
		return nil
	}

	// 使用超时上下文优雅关闭
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := s.server.Shutdown(ctx); err != nil {
		log.Debug().Err(err).Msg("Failed to shutdown HTTP server")
		return nil
	}

	log.Info().Msg("HTTP server stopped")
	return nil
}

func (s *Service) GetRouter() *gin.Engine {
	return s.router
}
