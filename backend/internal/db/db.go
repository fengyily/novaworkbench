// Package db provides the database connection layer for NovaWorkbench.
// It supports three drivers — SQLite (default, local-first), MySQL and
// PostgreSQL — behind a thin dialect wrapper around database/sql.
//
// Conventions for callers:
//   - Write SQL with `?` placeholders; DB.Exec/Query/QueryRow rebind them to
//     `$N` automatically when the active dialect is PostgreSQL.
//   - Use DB.Ident for identifiers that are reserved words on some dialects
//     (e.g. the `key` column, reserved in MySQL).
//   - Use DB.OnConflict to build dialect-correct upsert suffixes.
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// Dialect identifies the active SQL dialect.
type Dialect string

const (
	SQLite   Dialect = "sqlite"
	MySQL    Dialect = "mysql"
	Postgres Dialect = "postgres"
)

// DefaultSQLitePath is where the local-first database lives when nothing is
// configured.
const DefaultSQLitePath = "~/.novaworkbench/data/nova.db"

// configFilePath holds the DB config written from the settings UI. It is a
// file (not a row in the settings table) because the settings table itself
// lives inside the database — storing the DB config there would be circular.
const configFilePath = "~/.novaworkbench/dbconfig.json"

// ErrEnvManaged is returned by SaveConfig when the driver is pinned by the
// NOVA_DB_DRIVER environment variable — env always wins, so saving would be
// misleading.
var ErrEnvManaged = errors.New("database driver is managed by the NOVA_DB_DRIVER/NOVA_DB_DSN environment variables")

// Config selects the database driver and its connection parameters.
type Config struct {
	Driver     string `json:"driver"`      // "sqlite" | "mysql" | "postgres"
	DSN        string `json:"dsn"`         // mysql / postgres connection string
	SQLitePath string `json:"sqlite_path"` // sqlite file path
	Source     string `json:"-"`           // "env" | "file" | "default" (runtime only)
}

// LoadConfig resolves the DB config with the precedence:
// environment variables > dbconfig.json (settings UI) > sqlite default.
func LoadConfig() Config {
	if drv := os.Getenv("NOVA_DB_DRIVER"); drv != "" {
		return Config{
			Driver:     drv,
			DSN:        os.Getenv("NOVA_DB_DSN"),
			SQLitePath: envOr("NOVA_DB_PATH", DefaultSQLitePath),
			Source:     "env",
		}
	}
	if data, err := os.ReadFile(expandHome(configFilePath)); err == nil {
		var cfg Config
		if err := json.Unmarshal(data, &cfg); err == nil && cfg.Driver != "" {
			cfg.Source = "file"
			if cfg.Driver == string(SQLite) && cfg.SQLitePath == "" {
				cfg.SQLitePath = DefaultSQLitePath
			}
			return cfg
		}
	}
	return Config{Driver: string(SQLite), SQLitePath: DefaultSQLitePath, Source: "default"}
}

// SaveConfig persists the DB config chosen in the settings UI. It refuses
// when the driver is pinned by env vars.
func SaveConfig(cfg Config) error {
	if os.Getenv("NOVA_DB_DRIVER") != "" {
		return ErrEnvManaged
	}
	if err := os.MkdirAll(filepath.Dir(expandHome(configFilePath)), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(expandHome(configFilePath), data, 0600)
}

// BuildDSN composes a driver-specific DSN from structured fields (used by the
// settings UI so users never hand-write DSN syntax).
func BuildDSN(driver, host, port, user, password, dbname string) (string, error) {
	switch Dialect(driver) {
	case MySQL:
		if port == "" {
			port = "3306"
		}
		return fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&charset=utf8mb4&collation=utf8mb4_unicode_ci",
			user, password, host, port, dbname), nil
	case Postgres:
		if port == "" {
			port = "5432"
		}
		u := &url.URL{
			Scheme:   "postgres",
			User:     url.UserPassword(user, password),
			Host:     host + ":" + port,
			Path:     "/" + dbname,
			RawQuery: "sslmode=disable",
		}
		return u.String(), nil
	default:
		return "", fmt.Errorf("unsupported driver %q (want mysql or postgres)", driver)
	}
}

// MaskDSN hides the password in a DSN for display/logging.
func MaskDSN(dsn string) string {
	// URL form: postgres://user:pass@host/db
	if i := strings.Index(dsn, "://"); i >= 0 {
		if u, err := url.Parse(dsn); err == nil && u.User != nil {
			if _, has := u.User.Password(); has {
				u.User = url.UserPassword(u.User.Username(), "****")
				return u.String()
			}
		}
		return dsn
	}
	// MySQL form: user:pass@tcp(host:port)/db
	if i := strings.Index(dsn, "@"); i > 0 {
		if j := strings.Index(dsn[:i], ":"); j >= 0 {
			return dsn[:j+1] + "****" + dsn[i:]
		}
	}
	return dsn
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}

// DB wraps *sql.DB with the active dialect, rebinding placeholders and
// offering dialect helpers. Services hold *DB instead of *sql.DB; query call
// sites stay unchanged.
type DB struct {
	*sql.DB
	dialect Dialect
}

// Tx wraps a transaction with the same rebinding behavior as DB.
type Tx struct {
	*sql.Tx
	dialect Dialect
}

// Dialect returns the active dialect.
func (d *DB) Dialect() Dialect { return d.dialect }

// Rebind converts `?` placeholders to `$N` for PostgreSQL; other dialects use
// `?` natively so the query passes through unchanged.
func (d *DB) Rebind(query string) string {
	if d.dialect != Postgres {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteByte(query[i])
		}
	}
	return b.String()
}

// Ident quotes an identifier that may be a reserved word (notably `key`,
// reserved in MySQL). MySQL uses backticks; SQLite/Postgres use standard
// double quotes.
func (d *DB) Ident(name string) string {
	if d.dialect == MySQL {
		return "`" + name + "`"
	}
	return `"` + name + `"`
}

var excludedRef = regexp.MustCompile(`excluded\.(\w+)`)

// OnConflict builds an upsert suffix. setClause is written in portable form
// using `excluded.col` (the SQLite/Postgres spelling); for MySQL those
// references are rewritten to VALUES(col).
func (d *DB) OnConflict(target, setClause string) string {
	if d.dialect == MySQL {
		setClause = excludedRef.ReplaceAllString(setClause, "VALUES($1)")
		return " ON DUPLICATE KEY UPDATE " + setClause
	}
	return " ON CONFLICT(" + target + ") DO UPDATE SET " + setClause
}

// normalizeArgs adapts Go values pgx cannot encode into INTEGER columns.
// The SQLite/MySQL drivers implicitly map Go bools to 0/1; pgx refuses (bool
// has no binary encoding for int4), so the wrapper does it centrally.
// PostgreSQL-only — other dialects get the args unchanged.
func (d *DB) normalizeArgs(args []any) []any {
	if d.dialect != Postgres {
		return args
	}
	for i, a := range args {
		if b, ok := a.(bool); ok {
			if b {
				args[i] = int64(1)
			} else {
				args[i] = int64(0)
			}
		}
	}
	return args
}

// Exec / Query / QueryRow rebind then delegate.
func (d *DB) Exec(query string, args ...any) (sql.Result, error) {
	return d.DB.Exec(d.Rebind(query), d.normalizeArgs(args)...)
}

func (d *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return d.DB.Query(d.Rebind(query), d.normalizeArgs(args)...)
}

func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	return d.DB.QueryRow(d.Rebind(query), d.normalizeArgs(args)...)
}

// Begin starts a transaction whose statements are rebound the same way.
func (d *DB) Begin() (*Tx, error) {
	tx, err := d.DB.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, dialect: d.dialect}, nil
}

func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	d := &DB{dialect: t.dialect}
	return t.Tx.Exec(d.Rebind(query), d.normalizeArgs(args)...)
}

func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	d := &DB{dialect: t.dialect}
	return t.Tx.QueryRow(d.Rebind(query), d.normalizeArgs(args)...)
}

// Init opens the database described by cfg and runs schema migrations.
func Init(cfg Config) (*DB, error) {
	d := &DB{dialect: Dialect(cfg.Driver)}
	if d.dialect == "" {
		d.dialect = SQLite
	}

	var (
		raw *sql.DB
		err error
	)
	switch d.dialect {
	case SQLite:
		raw, err = openSQLite(cfg.SQLitePath)
	case MySQL, Postgres:
		if cfg.DSN == "" {
			return nil, fmt.Errorf("NOVA_DB_DSN (or saved DSN) is required for driver %q", d.dialect)
		}
		driverName := "mysql"
		if d.dialect == Postgres {
			driverName = "pgx"
		}
		raw, err = sql.Open(driverName, cfg.DSN)
	default:
		return nil, fmt.Errorf("unsupported NOVA_DB_DRIVER %q (want sqlite|mysql|postgres)", cfg.Driver)
	}
	if err != nil {
		return nil, err
	}
	d.DB = raw

	if d.dialect == SQLite {
		raw.SetMaxOpenConns(1) // SQLite single-writer
		raw.SetMaxIdleConns(1)
	} else {
		raw.SetMaxOpenConns(10)
		raw.SetMaxIdleConns(5)
		raw.SetConnMaxLifetime(5 * time.Minute)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := raw.PingContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("ping %s: %w", d.dialect, err)
	}

	if err := migrate(d); err != nil {
		raw.Close()
		return nil, fmt.Errorf("migrate %s: %w", d.dialect, err)
	}

	switch d.dialect {
	case SQLite:
		log.Printf("Database initialized: driver=sqlite path=%s", expandHome(cfg.SQLitePath))
	default:
		log.Printf("Database initialized: driver=%s dsn=%s", d.dialect, MaskDSN(cfg.DSN))
	}
	return d, nil
}

func openSQLite(path string) (*sql.DB, error) {
	path = expandHome(path)
	if path == "" {
		path = expandHome(DefaultSQLitePath)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	return sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
}

// OpenSQLite opens an existing SQLite file without running migrations — used
// as the copy source for one-shot data migration.
func OpenSQLite(path string) (*DB, error) {
	raw, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	raw.SetMaxOpenConns(1)
	return &DB{DB: raw, dialect: SQLite}, nil
}

// TestConnection opens, pings and reports the server version for a candidate
// config (settings UI "test connection" button). It does not migrate.
func TestConnection(driver, dsn string) (string, error) {
	driverName := "mysql"
	versionQuery := "SELECT VERSION()"
	if Dialect(driver) == Postgres {
		driverName = "pgx"
		versionQuery = "SHOW server_version"
	} else if Dialect(driver) != MySQL {
		return "", fmt.Errorf("unsupported driver %q (want mysql or postgres)", driver)
	}
	raw, err := sql.Open(driverName, dsn)
	if err != nil {
		return "", err
	}
	defer raw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := raw.PingContext(ctx); err != nil {
		return "", err
	}
	var version string
	if err := raw.QueryRowContext(ctx, versionQuery).Scan(&version); err != nil {
		return "", err
	}
	return version, nil
}
