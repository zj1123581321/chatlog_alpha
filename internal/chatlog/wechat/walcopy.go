package wechat

// walcopy.go：Step 5b WAL-aware copy + 一致性校验（architecture-rework-2026-05-06.md
// Eng Review Lock A2）。
//
// 复制顺序为何重要：
//   - SQLite WAL 协议下，.db 的 mxFrame 单调记录"已提交到 -wal 第 N 帧"。
//   - 若 .db 先复制，微信随后写入 -wal 添加新帧，再去复制 -wal 时拿到的版本
//     比 .db 知道的更新 → 解密产物里 .db 引用的帧可能 -wal 还没复制到 → SQLite
//     报 malformed。
//   - 反之 -wal 先复制：拿到的是较旧的 -wal；之后 .db 即便被前进，也只可能
//     "知道更少的帧"或与 -wal 一致，不会 dangling。
//
// 跳过 -shm：SQLite 在 open 时自动从 -wal 重建 wal-index；带 stale -shm 反而引入
// 一致性风险。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sjzar/chatlog/pkg/util"
)

// SQLite header 与 WAL header 的 magic 字节常量（SQLite 文件格式公开规范）。
const (
	sqliteMagicLen = 16
	walHeaderLen   = 32
)

var sqliteDBMagic = []byte("SQLite format 3\x00")

// WAL header magic：0x377f0682 表示原 db 是 little-endian，0x377f0683 表示 big-endian。
// 微信 db 实际是 little-endian，但保留两个 magic 让 CheckWALCoherency 不依赖端序假设。
const (
	walMagicLE = uint32(0x377f0682)
	walMagicBE = uint32(0x377f0683)
)

// DefaultWALMtimeWindow 是 .db 与 .db-wal mtime 偏差的默认容忍窗口。
// 实测微信 checkpoint 期间两文件 mtime 通常在 100ms 内，2s 给 OS / 文件系统抖动留余量。
const DefaultWALMtimeWindow = 2 * time.Second

// ErrWALIncoherent：WAL header 校验失败的统一 sentinel，调用方用 errors.Is 判定后
// 整个 generation 进 corrupt/。包含 InvalidDBMagic / InvalidWALMagic / MtimeOutOfWindow
// 三个子原因，errors.Is 树通过 Unwrap chain 都能匹配上。
var ErrWALIncoherent = errors.New("walcopy: incoherent db/wal pair")

// CopyDBPair 把 srcDB（.db）和它对应的 -wal（如存在）复制到 dst 路径。
// 顺序：-wal 先、.db 后。-shm 跳过。dst 目录必须事先存在（caller 责任）。
//
// 返回 walCopied=true 表示找到并复制了 -wal。
//
// 源用 util.OpenFileShared（FILE_SHARE_READ|WRITE|DELETE），与微信并行写入兼容。
// dst 用 os.OpenFile 普通独占创建（dst 在 work_dir/generations 下，没人会争）。
//
// 注意：CopyDBPair 只做物理复制，不做内容校验；校验由 CheckWALCoherency 单独负责，
// 让"复制失败"和"内容不一致"的语义在调用层分离。
func CopyDBPair(srcDB, dstDB string) (walCopied bool, err error) {
	srcWAL := srcDB + "-wal"
	dstWAL := dstDB + "-wal"

	// Step 1: -wal 先（如存在）
	if _, statErr := os.Stat(srcWAL); statErr == nil {
		if cerr := copySharedFile(srcWAL, dstWAL); cerr != nil {
			return false, fmt.Errorf("walcopy: copy wal: %w", cerr)
		}
		walCopied = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return false, fmt.Errorf("walcopy: stat wal: %w", statErr)
	}

	// Step 2: .db 后
	if cerr := copySharedFile(srcDB, dstDB); cerr != nil {
		return walCopied, fmt.Errorf("walcopy: copy db: %w", cerr)
	}
	return walCopied, nil
}

// copySharedFile 用 OpenFileShared 读源（兼容微信写锁）+ 标准 OpenFile 写目标。
// 写完 fsync 让 mtime + 内容落盘，下游 CheckWALCoherency 才能可靠看到 mtime。
func copySharedFile(src, dst string) error {
	in, err := util.OpenFileShared(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open dst: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy bytes: %w", err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("fsync dst: %w", err)
	}
	return nil
}

// CheckWALCoherency 对一对 (dbPath, walPath) 做轻量 header + mtime 一致性校验。
//
// 三层检查：
//  1. dbPath 起首 16 字节是 "SQLite format 3\x00" magic。
//  2. walPath 非空时存在 + 起首 4 字节是 WAL magic（0x377f0682 / 0x377f0683 BE）。
//  3. |db.mtime - wal.mtime| ≤ window（DefaultWALMtimeWindow=2s 对应日常 checkpoint 抖动）。
//
// 任一失败都 wrap 进 ErrWALIncoherent，调用方 errors.Is 即可统一处理（"整个 generation 进 corrupt/"）。
//
// walPath="" 跳过所有 WAL 相关检查（源文件原本就没有 -wal 的合法情形）。
//
// 不在 scope：salt-1/salt-2 的 frame-level 一致性校验。Eng Review 评估为 over-engineering：
// 微信不会在 chatlog 复制窗口内频繁 reset salt；mtime + magic + 复制顺序已经够 catch 实际 race。
func CheckWALCoherency(dbPath, walPath string, mtimeWindow time.Duration) error {
	dbStat, err := checkDBHeader(dbPath)
	if err != nil {
		return err
	}

	if walPath == "" {
		return nil
	}

	walStat, err := os.Stat(walPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// caller 传了 walPath 但文件不存在：当作"无 wal"通过。
			// 这与"原本就没 wal"等价（CopyDBPair 也可能在 stat 检查后被微信删掉 -wal）。
			return nil
		}
		return fmt.Errorf("walcopy: stat wal: %w", err)
	}

	if err := checkWALHeader(walPath); err != nil {
		return err
	}

	delta := dbStat.ModTime().Sub(walStat.ModTime())
	if delta < 0 {
		delta = -delta
	}
	if delta > mtimeWindow {
		return fmt.Errorf("%w: mtime delta %v > window %v", ErrWALIncoherent, delta, mtimeWindow)
	}
	return nil
}

func checkDBHeader(dbPath string) (os.FileInfo, error) {
	f, err := os.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("walcopy: open db: %w", err)
	}
	defer f.Close()

	head := make([]byte, sqliteMagicLen)
	if _, err := io.ReadFull(f, head); err != nil {
		return nil, fmt.Errorf("%w: read db magic: %v", ErrWALIncoherent, err)
	}
	if !bytes.Equal(head, sqliteDBMagic) {
		return nil, fmt.Errorf("%w: db magic mismatch", ErrWALIncoherent)
	}
	stat, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("walcopy: stat db: %w", err)
	}
	return stat, nil
}

func checkWALHeader(walPath string) error {
	f, err := os.Open(walPath)
	if err != nil {
		return fmt.Errorf("walcopy: open wal: %w", err)
	}
	defer f.Close()

	head := make([]byte, walHeaderLen)
	if _, err := io.ReadFull(f, head); err != nil {
		return fmt.Errorf("%w: read wal header: %v", ErrWALIncoherent, err)
	}
	magic := binary.BigEndian.Uint32(head[:4])
	if magic != walMagicLE && magic != walMagicBE {
		return fmt.Errorf("%w: wal magic %#x not recognized", ErrWALIncoherent, magic)
	}
	return nil
}
