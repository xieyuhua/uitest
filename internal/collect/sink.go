package collect

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	// 纯 Go 驱动：不依赖 CGO，Windows / Linux 同一份代码直接编译
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"

	"uitest-server/internal/model"
)

// 本文件是"落到哪里"：把提取出来的行写进数据库/文件。
//
// 支持 6 种 sink：
//   - sqlite   ：零外部依赖，默认写到 <数据目录>/harvest/harvest.db（纯 Go 驱动，Windows/Linux 通用）
//   - mysql    ：user:pass@tcp(host:3306)/db?charset=utf8mb4
//   - postgres ：postgres://user:pass@host:5432/db?sslmode=disable
//   - xlsx     ：Excel 表格 <数据目录>/harvest/<表名>.xlsx（可在控制台直接下载）
//   - json     ：JSON 数组文件 <数据目录>/harvest/<表名>.json（可在控制台直接下载）
//   - jsonl    ：追加写 JSON Lines（pandas / DuckDB / jq 直接可读，作为兜底）

// Sink 数据出口。
type Sink interface {
	// Write 写入一批行，返回成功写入行数。
	Write(rows []map[string]any) (int, error)
	// Describe 人可读的写入目标描述（写进运行日志）。
	Describe() string
	Close() error
}

// OpenSink 打开一个 sink。
func OpenSink(cfg model.HarvestSink, dataDir string) (Sink, error) {
	driver := strings.ToLower(strings.TrimSpace(cfg.Driver))
	if driver == "" {
		driver = "sqlite"
	}
	table := strings.TrimSpace(cfg.Table)
	if table == "" {
		table = "harvest_records"
	}
	batch := cfg.BatchSize
	if batch <= 0 {
		batch = 200
	}
	mode := "append"
	if strings.EqualFold(cfg.Mode, "upsert") {
		mode = "upsert"
	}
	switch driver {
	case "jsonl":
		dir := filepath.Join(dataDir, "harvest")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败：%w", err)
		}
		name := sanitizeFileName(table) + ".jsonl"
		return &jsonlSink{path: filepath.Join(dir, name)}, nil

	case "json":
		// JSON 文件：一个 JSON 数组，每次写入整份重写（小表最直观，浏览器/前端都能直接读）
		dir := filepath.Join(dataDir, "harvest")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败：%w", err)
		}
		path := filepath.Join(dir, sanitizeFileName(table)+".json")
		return &jsonFileSink{path: path, mode: mode, key: sanitizeColumn(cfg.KeyColumn)}, nil

	case "xlsx", "excel":
		dir := filepath.Join(dataDir, "harvest")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败：%w", err)
		}
		path := filepath.Join(dir, sanitizeFileName(table)+".xlsx")
		return newExcelSink(path, sanitizeFileName(table), mode, sanitizeColumn(cfg.KeyColumn)), nil

	case "sqlite":
		path := strings.TrimSpace(cfg.DSN)
		if path == "" {
			dir := filepath.Join(dataDir, "harvest")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("创建数据目录失败：%w", err)
			}
			path = filepath.Join(dir, "harvest.db")
		} else if !filepath.IsAbs(path) {
			path = filepath.Join(dataDir, "harvest", path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, fmt.Errorf("创建数据目录失败：%w", err)
			}
		}
		db, err := sql.Open("sqlite", path)
		if err != nil {
			return nil, fmt.Errorf("打开 SQLite 失败：%w", err)
		}
		// SQLite 单写者：把连接数压到 1，避免 database is locked
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		if err := db.Ping(); err != nil {
			db.Close()
			return nil, fmt.Errorf("连接 SQLite 失败（%s）：%w", path, err)
		}
		return newSQLSink(db, "sqlite", table, mode, cfg.KeyColumn, batch, "SQLite 数据库 "+path), nil

	case "mysql":
		if strings.TrimSpace(cfg.DSN) == "" {
			return nil, fmt.Errorf("MySQL 需要填写 dsn，例如 user:pass@tcp(127.0.0.1:3306)/test?charset=utf8mb4")
		}
		db, err := sql.Open("mysql", strings.TrimSpace(cfg.DSN))
		if err != nil {
			return nil, fmt.Errorf("打开 MySQL 失败：%w", err)
		}
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(2)
		db.SetConnMaxLifetime(30 * time.Minute)
		if err := db.Ping(); err != nil {
			db.Close()
			return nil, fmt.Errorf("连接 MySQL 失败：%w", err)
		}
		return newSQLSink(db, "mysql", table, mode, cfg.KeyColumn, batch, "MySQL 表 "+table), nil

	case "postgres", "postgresql", "pgsql":
		if strings.TrimSpace(cfg.DSN) == "" {
			return nil, fmt.Errorf("PostgreSQL 需要填写 dsn，例如 postgres://user:pass@127.0.0.1:5432/test?sslmode=disable")
		}
		db, err := sql.Open("postgres", strings.TrimSpace(cfg.DSN))
		if err != nil {
			return nil, fmt.Errorf("打开 PostgreSQL 失败：%w", err)
		}
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(2)
		if err := db.Ping(); err != nil {
			db.Close()
			return nil, fmt.Errorf("连接 PostgreSQL 失败：%w", err)
		}
		return newSQLSink(db, "postgres", table, mode, cfg.KeyColumn, batch, "PostgreSQL 表 "+table), nil

	default:
		return nil, fmt.Errorf("不支持的落库类型 %q（可选 sqlite / mysql / postgres / jsonl）", cfg.Driver)
	}
}

/* ------------------------------------------------------------------- SQL */

type sqlSink struct {
	db      *sql.DB
	dialect string
	table   string
	mode    string
	key     string
	batch   int
	desc    string

	mu      sync.Mutex
	known   map[string]bool
	created bool
}

func newSQLSink(db *sql.DB, dialect, table, mode, key string, batch int, desc string) *sqlSink {
	return &sqlSink{
		db: db, dialect: dialect, table: table, mode: mode,
		key: sanitizeColumn(key), batch: batch, desc: desc,
		known: map[string]bool{},
	}
}

func (s *sqlSink) Describe() string { return s.desc }

func (s *sqlSink) Close() error { return s.db.Close() }

func (s *sqlSink) quoteIdent(name string) string {
	switch s.dialect {
	case "mysql":
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	default:
		return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	}
}

func (s *sqlSink) placeholder(i int) string {
	if s.dialect == "postgres" {
		return "$" + fmt.Sprint(i+1)
	}
	return "?"
}

// columnType 列类型：整数→INTEGER，浮点→REAL，布尔→布尔，其余文本（JSON 嵌套存文本）。
func (s *sqlSink) columnType(rows []map[string]any, col string) string {
	kind := ""
	allInt := true
	for _, r := range rows {
		v, ok := r[col]
		if !ok || v == nil {
			continue
		}
		switch tv := v.(type) {
		case float64:
			if kind == "" || kind == "number" {
				kind = "number"
			}
			if tv != float64(int64(tv)) {
				allInt = false
			}
		case float32, int, int64, int32:
			if kind == "" || kind == "number" {
				kind = "number"
			}
		case bool:
			if kind == "" {
				kind = "bool"
			}
		default:
			kind = "text"
		}
		if kind == "text" {
			break
		}
	}
	switch kind {
	case "number":
		if allInt {
			if s.dialect == "mysql" {
				return "BIGINT"
			}
			return "INTEGER"
		}
		if s.dialect == "mysql" {
			return "DOUBLE"
		}
		if s.dialect == "postgres" {
			return "DOUBLE PRECISION"
		}
		return "REAL"
	case "bool":
		switch s.dialect {
		case "mysql":
			return "TINYINT(1)"
		case "postgres":
			return "BOOLEAN"
		default:
			return "INTEGER"
		}
	default:
		return "TEXT"
	}
}

// ensureSchema 建表 / 补列（字段后面新增也能自动跟上，不用手工改库）。
func (s *sqlSink) ensureSchema(rows []map[string]any) error {
	if !s.created {
		cols := unionColumns(rows)
		if len(cols) == 0 {
			return nil
		}
		var defs []string
		for _, c := range cols {
			defs = append(defs, s.quoteIdent(c)+" "+s.columnType(rows, c))
		}
		create := "CREATE TABLE IF NOT EXISTS " + s.quoteIdent(s.table) + " (" + strings.Join(defs, ", ") + ")"
		if _, err := s.db.Exec(create); err != nil {
			return fmt.Errorf("建表失败：%w", err)
		}
		s.created = true
		for _, c := range cols {
			s.known[c] = true
		}
		s.ensureKeyIndex()
		return nil
	}
	for _, c := range unionColumns(rows) {
		if s.known[c] {
			continue
		}
		alter := "ALTER TABLE " + s.quoteIdent(s.table) + " ADD COLUMN " + s.quoteIdent(c) + " " + s.columnType(rows, c)
		if _, err := s.db.Exec(alter); err != nil {
			// 列已存在（多进程并发）之类：忽略，后面插入会自然成功
			s.known[c] = true
			continue
		}
		s.known[c] = true
	}
	s.ensureKeyIndex()
	return nil
}

// ensureKeyIndex upsert 模式需要唯一键（建不上就算了，退化成追加）。
func (s *sqlSink) ensureKeyIndex() {
	if s.mode != "upsert" || s.key == "" || s.key == "col" {
		return
	}
	idx := "idx_" + s.table + "_" + s.key
	var stmt string
	switch s.dialect {
	case "mysql":
		stmt = "CREATE UNIQUE INDEX " + s.quoteIdent(idx) + " ON " + s.quoteIdent(s.table) + " (" + s.quoteIdent(s.key) + ")"
	default:
		stmt = "CREATE UNIQUE INDEX IF NOT EXISTS " + s.quoteIdent(idx) + " ON " + s.quoteIdent(s.table) + " (" + s.quoteIdent(s.key) + ")"
	}
	_, _ = s.db.Exec(stmt)
}

// buildInsert 生成 INSERT（upsert 模式带冲突处理）。
func (s *sqlSink) buildInsert(cols []string) string {
	var quoted, holders []string
	for i, c := range cols {
		quoted = append(quoted, s.quoteIdent(c))
		holders = append(holders, s.placeholder(i))
	}
	sql := "INSERT INTO " + s.quoteIdent(s.table) + " (" + strings.Join(quoted, ", ") + ") VALUES (" + strings.Join(holders, ", ") + ")"
	if s.mode != "upsert" || s.key == "" || len(cols) <= 1 {
		return sql
	}
	idx := -1
	for i, c := range cols {
		if c == s.key {
			idx = i
			break
		}
	}
	if idx < 0 {
		return sql
	}
	var updates []string
	for _, c := range cols {
		if c == s.key {
			continue
		}
		if s.dialect == "mysql" {
			updates = append(updates, s.quoteIdent(c)+"=VALUES("+s.quoteIdent(c)+")")
		} else {
			updates = append(updates, s.quoteIdent(c)+"=excluded."+s.quoteIdent(c))
		}
	}
	if len(updates) == 0 {
		return sql
	}
	switch s.dialect {
	case "mysql":
		return sql + " ON DUPLICATE KEY UPDATE " + strings.Join(updates, ", ")
	default:
		return sql + " ON CONFLICT (" + s.quoteIdent(s.key) + ") DO UPDATE SET " + strings.Join(updates, ", ")
	}
}

func (s *sqlSink) Write(rows []map[string]any) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureSchema(rows); err != nil {
		return 0, err
	}
	cols := unionColumns(rows)
	if len(cols) == 0 {
		return 0, nil
	}
	stmt := s.buildInsert(cols)
	written := 0
	batch := s.batch
	if batch <= 0 {
		batch = 200
	}
	for start := 0; start < len(rows); start += batch {
		end := start + batch
		if end > len(rows) {
			end = len(rows)
		}
		tx, err := s.db.Begin()
		if err != nil {
			return written, fmt.Errorf("开启事务失败：%w", err)
		}
		bad := false
		for _, row := range rows[start:end] {
			args := make([]any, len(cols))
			for i, c := range cols {
				args[i] = normalizeValue(row[c], s.dialect)
			}
			if _, err := tx.Exec(stmt, args...); err != nil {
				_ = tx.Rollback()
				return written, fmt.Errorf("写入失败：%w", err)
			}
			written++
		}
		if bad {
			_ = tx.Rollback()
			continue
		}
		if err := tx.Commit(); err != nil {
			return written, fmt.Errorf("提交事务失败：%w", err)
		}
	}
	return written, nil
}

/* ------------------------------------------------------------------ JSONL */

type jsonlSink struct {
	path string
	mu   sync.Mutex
}

func (s *jsonlSink) Describe() string { return "JSON Lines 文件 " + s.path }

func (s *jsonlSink) Close() error { return nil }

func (s *jsonlSink) Write(rows []map[string]any) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("打开文件失败：%w", err)
	}
	defer f.Close()
	w := 0
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			continue
		}
		if _, err := f.Write(append(raw, '\n')); err != nil {
			return w, fmt.Errorf("写入文件失败：%w", err)
		}
		w++
	}
	return w, nil
}

/* ---------------------------------------------------------------- 工具函数 */

func unionColumns(rows []map[string]any) []string {
	set := map[string]bool{}
	for _, r := range rows {
		for k := range r {
			set[k] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// normalizeValue 把 JSON 值转成驱动能接受的类型。
func normalizeValue(v any, dialect string) any {
	switch x := v.(type) {
	case nil:
		return nil
	case bool:
		if dialect == "postgres" {
			return x
		}
		if x {
			return 1
		}
		return 0
	case float64, float32, int, int64, int32, string:
		return x
	default:
		if raw, err := json.Marshal(x); err == nil {
			return string(raw)
		}
		return fmt.Sprint(x)
	}
}

func sanitizeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		case r >= 0x4e00 && r <= 0x9fa5:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		out = "harvest_records"
	}
	return out
}
