package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/novaworkbench/backend/internal/db"
)

// DatabaseHandler serves the database settings UI: current driver info,
// connection testing, saving a new target (dbconfig.json), and one-shot data
// migration from the running SQLite database to a configured MySQL/Postgres
// target. Switching to the saved config always requires a restart — the
// handler never hot-swaps the live handle.
type DatabaseHandler struct {
	database *db.DB
	cfg      db.Config // the config the server was started with
}

func NewDatabaseHandler(database *db.DB, cfg db.Config) *DatabaseHandler {
	return &DatabaseHandler{database: database, cfg: cfg}
}

type databaseInfo struct {
	Driver     string `json:"driver"`
	DSNMasked  string `json:"dsn_masked"`
	Source     string `json:"source"` // "env" | "file" | "default"
	SQLitePath string `json:"sqlite_path"`
}

// Get returns the active database configuration (DSN password masked).
func (h *DatabaseHandler) Get(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, databaseInfo{
		Driver:     string(h.database.Dialect()),
		DSNMasked:  db.MaskDSN(h.cfg.DSN),
		Source:     h.cfg.Source,
		SQLitePath: h.cfg.SQLitePath,
	})
}

type databaseConnReq struct {
	Driver   string `json:"driver"`
	Host     string `json:"host"`
	Port     string `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	DBName   string `json:"dbname"`
}

func (h *DatabaseHandler) buildAndTest(req databaseConnReq) (string, string, error) {
	dsn, err := db.BuildDSN(req.Driver, req.Host, req.Port, req.User, req.Password, req.DBName)
	if err != nil {
		return "", "", err
	}
	version, err := db.TestConnection(req.Driver, dsn)
	if err != nil {
		return dsn, "", err
	}
	return dsn, version, nil
}

// Test checks a candidate connection without saving anything.
func (h *DatabaseHandler) Test(w http.ResponseWriter, r *http.Request) {
	var req databaseConnReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "请求体不是合法 JSON")
		return
	}
	_, version, err := h.buildAndTest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CONN_FAILED", "连接失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
}

// Save validates the connection, then persists it to dbconfig.json. The new
// database takes effect on the next server restart.
func (h *DatabaseHandler) Save(w http.ResponseWriter, r *http.Request) {
	var req databaseConnReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "请求体不是合法 JSON")
		return
	}
	dsn, _, err := h.buildAndTest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CONN_FAILED", "连接失败，配置未保存: "+err.Error())
		return
	}
	cfg := db.Config{Driver: req.Driver, DSN: dsn}
	if err := db.SaveConfig(cfg); err != nil {
		if errors.Is(err, db.ErrEnvManaged) {
			writeError(w, http.StatusConflict, "ENV_MANAGED",
				"当前数据库由环境变量 NOVA_DB_DRIVER/NOVA_DB_DSN 指定，请修改环境变量后重启")
			return
		}
		writeError(w, http.StatusInternalServerError, "SAVE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restart_required": true})
}

// Migrate copies all data from the running SQLite database into the
// configured MySQL/Postgres target (env or dbconfig.json), then returns a
// per-table row report. The server keeps running on SQLite; restart to switch.
func (h *DatabaseHandler) Migrate(w http.ResponseWriter, r *http.Request) {
	if h.database.Dialect() != db.SQLite {
		writeError(w, http.StatusBadRequest, "NOT_SQLITE",
			"当前运行的数据库不是 SQLite（已是 "+string(h.database.Dialect())+"），无需迁移")
		return
	}
	cfg := db.LoadConfig()
	if db.Dialect(cfg.Driver) == db.SQLite || cfg.Driver == "" {
		writeError(w, http.StatusBadRequest, "NO_TARGET",
			"尚未配置目标数据库，请先保存 MySQL/PostgreSQL 连接配置")
		return
	}

	dst, err := db.Init(cfg) // opens + creates schema on the target
	if err != nil {
		writeError(w, http.StatusBadRequest, "TARGET_UNREACHABLE", "目标库连接/建表失败: "+err.Error())
		return
	}
	defer dst.Close()

	stats, err := db.Migrate(h.database, dst, func(msg string) { log.Printf("[migrate] %s", msg) })
	if err != nil {
		writeError(w, http.StatusInternalServerError, "MIGRATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tables":           stats,
		"target_driver":    cfg.Driver,
		"restart_required": true,
	})
}
