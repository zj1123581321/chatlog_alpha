package dbm

import (
	"database/sql"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog/log"

	"github.com/sjzar/chatlog/internal/errors"
	"github.com/sjzar/chatlog/pkg/filecopy"
	"github.com/sjzar/chatlog/pkg/filemonitor"
)

type DBManager struct {
	path       string
	id         string
	walEnabled bool
	fm         *filemonitor.FileMonitor
	fgs        map[string]*filemonitor.FileGroup
	dbs        map[string]*sql.DB
	dbPaths    map[string][]string
	mutex      sync.RWMutex
}

func NewDBManager(path string, walEnabled bool) *DBManager {
	return &DBManager{
		path:       path,
		id:         filepath.Base(path),
		walEnabled: walEnabled,
		fm:         filemonitor.NewFileMonitor(),
		fgs:        make(map[string]*filemonitor.FileGroup),
		dbs:        make(map[string]*sql.DB),
		dbPaths:    make(map[string][]string),
	}
}

func (d *DBManager) AddGroup(g *Group) error {
	fg, err := filemonitor.NewFileGroup(g.Name, d.path, g.Pattern, g.BlackList)
	if err != nil {
		return err
	}
	fg.AddCallback(d.Callback)
	d.fm.AddGroup(fg)
	d.mutex.Lock()
	d.fgs[g.Name] = fg
	d.mutex.Unlock()
	return nil
}

func (d *DBManager) AddCallback(group string, callback func(event fsnotify.Event) error) error {
	d.mutex.RLock()
	fg, ok := d.fgs[group]
	d.mutex.RUnlock()
	if !ok {
		return errors.FileGroupNotFound(group)
	}
	fg.AddCallback(callback)
	return nil
}

func (d *DBManager) GetDB(name string) (*sql.DB, error) {
	dbPaths, err := d.GetDBPath(name)
	if err != nil {
		return nil, err
	}
	return d.OpenDB(dbPaths[0])
}

func (d *DBManager) GetDBs(name string) ([]*sql.DB, error) {
	dbPaths, err := d.GetDBPath(name)
	if err != nil {
		return nil, err
	}
	dbs := make([]*sql.DB, 0)
	for _, file := range dbPaths {
		db, err := d.OpenDB(file)
		if err != nil {
			return nil, err
		}
		dbs = append(dbs, db)
	}
	return dbs, nil
}

func (d *DBManager) GetDBPath(name string) ([]string, error) {
	d.mutex.RLock()
	dbPaths, ok := d.dbPaths[name]
	d.mutex.RUnlock()
	if !ok {
		d.mutex.RLock()
		fg, ok := d.fgs[name]
		d.mutex.RUnlock()
		if !ok {
			return nil, errors.FileGroupNotFound(name)
		}
		list, err := fg.List()
		if err != nil {
			return nil, errors.DBFileNotFound(d.path, fg.PatternStr, err)
		}
		if len(list) == 0 {
			return nil, errors.DBFileNotFound(d.path, fg.PatternStr, nil)
		}
		dbPaths = filterPrimaryDBs(list)
		d.mutex.Lock()
		d.dbPaths[name] = dbPaths
		d.mutex.Unlock()
	}
	return dbPaths, nil
}

func (d *DBManager) OpenDB(path string) (*sql.DB, error) {
	d.mutex.RLock()
	db, ok := d.dbs[path]
	d.mutex.RUnlock()
	if ok {
		return db, nil
	}
	var err error
	tempPath := path
	if runtime.GOOS == "windows" {
		tempPath, err = filecopy.GetTempCopy(d.id, path)
		if err != nil {
			log.Err(err).Msgf("获取临时拷贝文件 %s 失败", path)
			return nil, err
		}
	}
	db, err = sql.Open("sqlite3", tempPath)
	if err != nil {
		log.Err(err).Msgf("连接数据库 %s 失败", path)
		return nil, err
	}
	// 限制 SQLite 连接池：chatlog 场景下并发 HTTP 请求很少真正并行查同一个库，
	// 无上限会在高并发抖动时打开数十个 SQLite 连接，每个连接又会吃
	// 3 个文件 HANDLE（.db / -wal / -shm）。ConnMaxIdleTime 让空闲连接
	// 在几分钟后主动释放，避免 idle 连接长期 hold filecopy 临时文件。
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	d.mutex.Lock()
	d.dbs[path] = db
	d.mutex.Unlock()
	return db, nil
}

func (d *DBManager) Callback(event fsnotify.Event) error {
	if !(event.Op.Has(fsnotify.Create) || event.Op.Has(fsnotify.Write) || event.Op.Has(fsnotify.Rename)) {
		return nil
	}

	basePath := normalizeDBPath(event.Name)
	d.mutex.Lock()
	db, ok := d.dbs[basePath]
	if ok {
		delete(d.dbs, basePath)
		go func(db *sql.DB) {
			time.Sleep(time.Second * 5)
			db.Close()
		}(db)
	}
	if (event.Op.Has(fsnotify.Create) || event.Op.Has(fsnotify.Rename)) && isPrimaryDBFile(event.Name) {
		d.dbPaths = make(map[string][]string)
	}
	d.mutex.Unlock()

	return nil
}

func (d *DBManager) Start() error {
	return d.fm.Start()
}

func (d *DBManager) Stop() error {
	return d.fm.Stop()
}

func (d *DBManager) Close() error {
	for _, db := range d.dbs {
		db.Close()
	}
	return d.fm.Stop()
}

// InvalidateAll 关掉所有缓存的 sql.DB 连接 + 清 dbs/dbPaths 两张 map。
//
// Step 5 generation 切换的服务端 hook（spec Eng Review Lock A3）：watcher 完成
// atomic swap 后，server 进程 30s 内 polling status.json 检出 current_generation
// 变化，调用本方法。下一次 GetDB / OpenDB 会按新 generation 路径 sql.Open，
// 池中的连接不会再指向旧 generation 的物理文件。
//
// 关闭走 goroutine 异步：在途 query 还在用 fd 时同步 Close 会阻塞 invalidate 线程；
// 异步释放让本调用立刻返回，pool 切换不被慢查询拖住。
func (d *DBManager) InvalidateAll() {
	d.mutex.Lock()
	dbs := d.dbs
	d.dbs = make(map[string]*sql.DB)
	d.dbPaths = make(map[string][]string)
	d.mutex.Unlock()

	for _, db := range dbs {
		go func(db *sql.DB) {
			_ = db.Close()
		}(db)
	}
}

func normalizeDBPath(path string) string {
	if strings.HasSuffix(path, "-wal") || strings.HasSuffix(path, "-shm") {
		return strings.TrimSuffix(strings.TrimSuffix(path, "-wal"), "-shm")
	}
	return path
}

func isPrimaryDBFile(path string) bool {
	return strings.HasSuffix(path, ".db") && !strings.HasSuffix(path, ".db-wal") && !strings.HasSuffix(path, ".db-shm")
}

func filterPrimaryDBs(paths []string) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if isPrimaryDBFile(path) {
			result = append(result, path)
		}
	}
	return result
}
