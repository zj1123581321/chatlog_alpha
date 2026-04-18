package ctx

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sjzar/chatlog/internal/chatlog/conf"
	"github.com/sjzar/chatlog/internal/wechat"
	"github.com/sjzar/chatlog/pkg/config"
	"github.com/sjzar/chatlog/pkg/filecopy"
	"github.com/sjzar/chatlog/pkg/util"
)

const (
	DefalutHTTPAddr = "127.0.0.1:5030"
)

// Snapshot holds a read-only copy of all UI-visible Context fields.
// Obtained via GetSnapshot() with a single RLock, used by the TUI refresh loop.
type Snapshot struct {
	Account             string
	PID                 int
	FullVersion         string
	ExePath             string
	Status              string
	DataKey             string
	ImgKey              string
	Platform            string
	DataUsage           string
	DataDir             string
	WorkUsage           string
	WorkDir             string
	HTTPAddr            string
	HTTPEnabled         bool
	AutoDecrypt         bool
	AutoDecryptDebounce int
	WalEnabled          bool
	LastSession         time.Time
}

// Context is the shared application state for chatlog.
// All fields are private and must be accessed through thread-safe getter/setter methods.
type Context struct {
	conf *conf.TUIConfig
	cm   *config.Manager
	mu   sync.RWMutex

	history map[string]conf.ProcessConfig

	// wechat account state
	account     string
	platform    string
	version     int
	fullVersion string
	dataDir     string
	dataKey     string
	dataUsage   string
	imgKey      string

	// work directory state
	workDir   string
	workUsage string

	// HTTP service state
	httpEnabled bool
	httpAddr    string

	// auto decrypt
	autoDecrypt         bool
	lastSession         time.Time
	walEnabled          bool
	autoDecryptDebounce int

	// current wechat instance
	current *wechat.Account
	pid     int
	exePath string
	status  string

	// all available wechat instances
	weChatInstances []*wechat.Account
}

func New(configPath string) (*Context, error) {

	conf, tcm, err := conf.LoadTUIConfig(configPath)
	if err != nil {
		return nil, err
	}

	ctx := &Context{
		conf:     conf,
		cm:       tcm,
		httpAddr: DefalutHTTPAddr,
	}

	ctx.loadConfig()

	return ctx, nil
}

func (c *Context) loadConfig() {
	c.history = c.conf.ParseHistory()
	c.switchHistory(c.conf.LastAccount)
	c.refresh()
}

// --- Public lock-acquiring entry points ---

func (c *Context) SwitchHistory(account string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.switchHistory(account)
}

func (c *Context) SwitchCurrent(info *wechat.Account) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.switchHistory(info.Name)
	c.current = info
	c.refresh()
}

func (c *Context) Refresh() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refresh()
}

func (c *Context) UpdateConfig() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updateConfig()
}

// --- Private lock-free internal methods (caller must hold mu) ---

func (c *Context) switchHistory(account string) {
	c.current = nil
	c.pid = 0
	c.exePath = ""
	c.status = ""
	history, ok := c.history[account]
	if ok {
		c.account = history.Account
		c.platform = history.Platform
		c.version = history.Version
		c.fullVersion = history.FullVersion
		c.dataKey = history.DataKey
		c.imgKey = history.ImgKey
		c.dataDir = history.DataDir
		c.workDir = history.WorkDir
		c.httpEnabled = history.HTTPEnabled
		c.httpAddr = history.HTTPAddr
		c.walEnabled = history.WalEnabled
		c.autoDecrypt = history.AutoDecrypt
		c.autoDecryptDebounce = history.AutoDecryptDebounce
	} else {
		c.account = ""
		c.platform = ""
		c.version = 0
		c.fullVersion = ""
		c.dataKey = ""
		c.imgKey = ""
		c.dataDir = ""
		c.workDir = ""
		c.httpEnabled = false
		c.httpAddr = ""
		c.walEnabled = false
		c.autoDecrypt = false
		c.autoDecryptDebounce = 0
	}
	if c.httpAddr == "" {
		c.httpAddr = DefalutHTTPAddr
	}
}

func (c *Context) refresh() {
	if c.current != nil {
		c.account = c.current.Name
		c.platform = c.current.Platform
		c.version = c.current.Version
		c.fullVersion = c.current.FullVersion
		c.pid = int(c.current.PID)
		c.exePath = c.current.ExePath
		c.status = c.current.Status
		if c.current.Key != "" {
			c.dataKey = c.current.Key
		}
		if c.current.ImgKey != "" {
			c.imgKey = c.current.ImgKey
		}
		if c.current.DataDir != c.dataDir {
			c.dataDir = c.current.DataDir
		}
	}
	if c.dataUsage == "" && c.dataDir != "" {
		dataDir := c.dataDir
		go func() {
			size := util.GetDirSize(dataDir)
			c.mu.Lock()
			c.dataUsage = size
			c.mu.Unlock()
		}()
	}
	if c.workUsage == "" && c.workDir != "" {
		workDir := c.workDir
		go func() {
			workSize := util.GetDirSize(workDir)
			cacheDir := filecopy.GetCacheDir()
			cacheSize := util.GetDirSize(cacheDir)
			result := fmt.Sprintf("%s (Cache: %s)", workSize, cacheSize)
			c.mu.Lock()
			c.workUsage = result
			c.mu.Unlock()
		}()
	}
}

func (c *Context) updateConfig() {

	pconf := conf.ProcessConfig{
		Type:                "wechat",
		Account:             c.account,
		Platform:            c.platform,
		Version:             c.version,
		FullVersion:         c.fullVersion,
		DataDir:             c.dataDir,
		DataKey:             c.dataKey,
		ImgKey:              c.imgKey,
		WorkDir:             c.workDir,
		HTTPEnabled:         c.httpEnabled,
		HTTPAddr:            c.httpAddr,
		WalEnabled:          c.walEnabled,
		AutoDecrypt:         c.autoDecrypt,
		AutoDecryptDebounce: c.autoDecryptDebounce,
	}

	if c.conf.History == nil {
		c.conf.History = make([]conf.ProcessConfig, 0)
	}
	if len(c.conf.History) == 0 {
		c.conf.History = append(c.conf.History, pconf)
	} else {
		isFind := false
		for i, v := range c.conf.History {
			if v.Account == c.account {
				isFind = true
				c.conf.History[i] = pconf
				break
			}
		}
		if !isFind {
			c.conf.History = append(c.conf.History, pconf)
		}
	}

	if err := c.cm.SetConfig("last_account", c.account); err != nil {
		log.Error().Err(err).Msg("set last_account failed")
		return
	}

	if err := c.cm.SetConfig("history", c.conf.History); err != nil {
		log.Error().Err(err).Msg("set history failed")
		return
	}

	if len(pconf.DataDir) != 0 {
		if b, err := json.Marshal(pconf); err == nil {
			if err := os.WriteFile(filepath.Join(pconf.DataDir, "chatlog.json"), b, 0644); err != nil {
				log.Error().Err(err).Msg("save chatlog.json failed")
			}
		}
	}
}

// --- Getters (all RLock-protected) ---

func (c *Context) GetAccount() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.account
}

func (c *Context) GetPlatform() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.platform
}

func (c *Context) GetVersion() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.version
}

func (c *Context) GetFullVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fullVersion
}

func (c *Context) GetDataDir() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dataDir
}

func (c *Context) GetDataKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dataKey
}

func (c *Context) GetDataUsage() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dataUsage
}

func (c *Context) GetImgKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.imgKey
}

func (c *Context) GetWorkDir() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.workDir
}

func (c *Context) GetWorkUsage() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.workUsage
}

func (c *Context) GetHTTPEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.httpEnabled
}

func (c *Context) GetHTTPAddr() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.httpAddr
}

func (c *Context) GetAutoDecrypt() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.autoDecrypt
}

func (c *Context) GetLastSession() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastSession
}

func (c *Context) GetWalEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.walEnabled
}

func (c *Context) GetAutoDecryptDebounce() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.autoDecryptDebounce
}

func (c *Context) GetPID() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pid
}

func (c *Context) GetExePath() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.exePath
}

func (c *Context) GetStatus() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

// GetCurrent returns a shallow copy of the current wechat account, or nil.
func (c *Context) GetCurrent() *wechat.Account {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.current == nil {
		return nil
	}
	cp := *c.current
	return &cp
}

// GetWeChatInstances returns a copy of the instances slice.
func (c *Context) GetWeChatInstances() []*wechat.Account {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]*wechat.Account, len(c.weChatInstances))
	copy(result, c.weChatInstances)
	return result
}

// GetHistory returns a copy of the history map.
func (c *Context) GetHistory() map[string]conf.ProcessConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make(map[string]conf.ProcessConfig, len(c.history))
	for k, v := range c.history {
		result[k] = v
	}
	return result
}

// GetSnapshot returns a read-only snapshot of all UI-visible fields in a single lock acquisition.
func (c *Context) GetSnapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Snapshot{
		Account:             c.account,
		PID:                 c.pid,
		FullVersion:         c.fullVersion,
		ExePath:             c.exePath,
		Status:              c.status,
		DataKey:             c.dataKey,
		ImgKey:              c.imgKey,
		Platform:            c.platform,
		DataUsage:           c.dataUsage,
		DataDir:             c.dataDir,
		WorkUsage:           c.workUsage,
		WorkDir:             c.workDir,
		HTTPAddr:            c.httpAddr,
		HTTPEnabled:         c.httpEnabled,
		AutoDecrypt:         c.autoDecrypt,
		AutoDecryptDebounce: c.autoDecryptDebounce,
		WalEnabled:          c.walEnabled,
		LastSession:         c.lastSession,
	}
}

func (c *Context) GetWebhook() *conf.Webhook {
	return c.conf.Webhook
}

func (c *Context) GetSaveDecryptedMedia() bool {
	return true
}

// GetBackupPath returns the backup directory path. Prefers env var CHATLOG_BACKUP_PATH.
func (c *Context) GetBackupPath() string {
	if v := os.Getenv("CHATLOG_BACKUP_PATH"); v != "" {
		return v
	}
	return c.conf.BackupPath
}

// GetBackupFolderMap returns the talker → folder_id (hex) mapping for backup lookups.
// Only used when the backup directory uses the hex folder_id naming convention
// (e.g. "群名(C606ACFA)"); @chatroom-suffixed directories are resolved automatically.
func (c *Context) GetBackupFolderMap() map[string]string {
	return c.conf.BackupFolderMap
}

// --- Setters (all Lock-protected) ---

func (c *Context) SetHTTPEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.httpEnabled == enabled {
		return
	}
	c.httpEnabled = enabled
	c.updateConfig()
}

func (c *Context) SetHTTPAddr(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.httpAddr == addr {
		return
	}
	c.httpAddr = addr
	c.updateConfig()
}

func (c *Context) SetWorkDir(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.workDir == dir {
		return
	}
	c.workDir = dir
	c.updateConfig()
	c.refresh()
}

func (c *Context) SetDataDir(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dataDir == dir {
		return
	}
	c.dataDir = dir
	c.updateConfig()
	c.refresh()
}

func (c *Context) SetDataKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dataKey == key {
		return
	}
	c.dataKey = key
	c.updateConfig()
}

func (c *Context) SetImgKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.imgKey == key {
		return
	}
	c.imgKey = key
	c.updateConfig()
}

func (c *Context) SetAutoDecrypt(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.autoDecrypt == enabled {
		return
	}
	c.autoDecrypt = enabled
	c.updateConfig()
}

func (c *Context) SetWalEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.walEnabled == enabled {
		return
	}
	c.walEnabled = enabled
	c.updateConfig()
}

func (c *Context) SetAutoDecryptDebounce(debounce int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.autoDecryptDebounce == debounce {
		return
	}
	c.autoDecryptDebounce = debounce
	c.updateConfig()
}

func (c *Context) SetLastSession(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSession = t
}

func (c *Context) SetWeChatInstances(instances []*wechat.Account) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.weChatInstances = instances
}

func (c *Context) SetBackupPath(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conf.BackupPath == path {
		return
	}
	c.conf.BackupPath = path
	if err := c.cm.SetConfig("backup_path", path); err != nil {
		log.Error().Err(err).Msg("set backup_path failed")
	}
}
