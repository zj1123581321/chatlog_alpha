package wechat

// schema_gate.go：Step 5c Schema Gate 三层校验（architecture-rework-2026-05-06.md
// Eng Review Lock A6）。
//
// 校验顺序（quick 模式）：
//  1. PRAGMA table_info(<expected_table>) —— 业务级 schema：表必须存在。catch 4/25
//     "no such table: Timestamp" 那类损坏（race + 半写入 schema）。
//  2. SmokeQuery —— 对 hot table 跑一次 SELECT count(*)；触发 SQLite 数据 page
//     的实际访问，破损 page 会抛 SQLITE_CORRUPT。catch 数据 page mid-write 损坏。
//  3. PRAGMA quick_check(50) —— 通用结构损坏兜底，参数 50 限制最多 50 个 error
//     提前返回。catch 索引/page 链结构破损。
//
// full 模式：用 PRAGMA integrity_check 替代第 3 层（覆盖更全但 1-3min/db，
// 仅 nightly 诊断或人工触发，不在 polling 路径用）。
//
// 接受的 false negative：index 与 data 不一致（quick_check 不验）；UNIQUE/NOT NULL
// 违反（chatlog 是 read-only，微信不在这些 db 上写故不会发生）。
//
// 任意层失败：整个 generation 进 corrupt/，调用方 errors.Is(err, ErrSchemaGateFailed) 判定。

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SchemaCheckMode：quick=三层组合（默认，~5s/50dbs）；full=三层但末层换 integrity_check（小时级，nightly）。
type SchemaCheckMode string

const (
	SchemaCheckQuick SchemaCheckMode = "quick"
	SchemaCheckFull  SchemaCheckMode = "full"
)

// DefaultSchemaCheckMode 是 spec §3.2 的默认 mode。
const DefaultSchemaCheckMode = SchemaCheckQuick

// SchemaSpec 描述对单个 db 的业务级 schema 期望。caller 在 5d 集成时按 db 类型构造。
//
// ExpectedTables：必须存在的表名列表；任一缺失即 fail。空列表跳过表存在性检查。
// SmokeQuery：对 hot table 的轻量 select；触发数据 page 实际读取。空字符串跳过 smoke。
type SchemaSpec struct {
	ExpectedTables []string
	SmokeQuery     string
}

// ErrSchemaGateFailed 是所有 schema gate 失败的统一 sentinel。Wrap 上具体细节后返回。
var ErrSchemaGateFailed = errors.New("schema gate: failed")

// ValidateSchemaCheckMode 把字符串转成 SchemaCheckMode，配合 spec §3.2
// "--schema-check-mode = quick|full" 配置项的输入校验。
func ValidateSchemaCheckMode(s string) (SchemaCheckMode, error) {
	switch SchemaCheckMode(s) {
	case SchemaCheckQuick, SchemaCheckFull:
		return SchemaCheckMode(s), nil
	default:
		return "", fmt.Errorf("schema-check-mode: invalid value %q (want %q or %q)",
			s, SchemaCheckQuick, SchemaCheckFull)
	}
}

// CheckSchema 对解密后的 db 跑三层校验。dbPath 必须是已解密的 SQLite 文件。
//
// 任意层失败 → 返回包装 ErrSchemaGateFailed 的错误，调用方 errors.Is 判定后路由到 corrupt/。
// 严格语义：第一层 fail 后立刻返回，不再跑后续（节省时间，且后续大概率会因为同一根因继续 fail）。
func CheckSchema(dbPath string, spec SchemaSpec, mode SchemaCheckMode) error {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return fmt.Errorf("schema gate: open: %w", err)
	}
	defer db.Close()

	// Layer 1: business-level table existence
	for _, table := range spec.ExpectedTables {
		if err := assertTableExists(db, table); err != nil {
			return err
		}
	}

	// Layer 2: smoke query (data page touch)
	if spec.SmokeQuery != "" {
		if err := runSmoke(db, spec.SmokeQuery); err != nil {
			return err
		}
	}

	// Layer 3: structural check
	switch mode {
	case SchemaCheckFull:
		if err := runIntegrityCheck(db); err != nil {
			return err
		}
	default: // SchemaCheckQuick or unset → quick
		if err := runQuickCheck(db); err != nil {
			return err
		}
	}
	return nil
}

func assertTableExists(db *sql.DB, table string) error {
	// PRAGMA table_info 返回的列数 = table 列数。表不存在时返回 0 行。
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", quoteIdent(table)))
	if err != nil {
		return fmt.Errorf("%w: PRAGMA table_info(%s): %v", ErrSchemaGateFailed, table, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return fmt.Errorf("%w: missing required table %q", ErrSchemaGateFailed, table)
	}
	return nil
}

func runSmoke(db *sql.DB, query string) error {
	rows, err := db.Query(query)
	if err != nil {
		return fmt.Errorf("%w: smoke query: %v", ErrSchemaGateFailed, err)
	}
	defer rows.Close()
	// 不关心结果，只要 query 不报错；rows.Err() 会暴露执行期破损。
	for rows.Next() {
		var discard interface{}
		_ = rows.Scan(&discard)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: smoke iter: %v", ErrSchemaGateFailed, err)
	}
	return nil
}

// runQuickCheck 跑 PRAGMA quick_check(50)。结果是一组字符串行；good db 返回单行 "ok"，
// 损坏返回若干 error 描述行。我们检 "第一行 == ok 且只有一行"。
func runQuickCheck(db *sql.DB) error {
	rows, err := db.Query("PRAGMA quick_check(50)")
	if err != nil {
		return fmt.Errorf("%w: quick_check: %v", ErrSchemaGateFailed, err)
	}
	defer rows.Close()
	return interpretCheckRows(rows, "quick_check")
}

func runIntegrityCheck(db *sql.DB) error {
	rows, err := db.Query("PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("%w: integrity_check: %v", ErrSchemaGateFailed, err)
	}
	defer rows.Close()
	return interpretCheckRows(rows, "integrity_check")
}

func interpretCheckRows(rows *sql.Rows, label string) error {
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("%w: %s scan: %v", ErrSchemaGateFailed, label, err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: %s iter: %v", ErrSchemaGateFailed, label, err)
	}
	if len(lines) == 1 && lines[0] == "ok" {
		return nil
	}
	return fmt.Errorf("%w: %s reported: %s",
		ErrSchemaGateFailed, label, strings.Join(lines, "; "))
}

// quoteIdent 把 SQLite identifier 包成双引号并转义内嵌双引号。
// 用于 PRAGMA table_info(<ident>)。caller 传的 table 名来自 chatlog 自己维护的常量列表，
// 不来自外部输入；quote 只是保险，避免特殊字符导致语法错。
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
