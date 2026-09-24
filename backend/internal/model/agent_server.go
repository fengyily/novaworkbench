package model

// AgentServer represents a remote execution target. The credential stored in
// DB (auth_value) is an AES-256-GCM ciphertext produced by internal/secret;
// the in-memory model only ever holds the plaintext after a successful
// GetWithCredential call, and that string is never serialized to JSON.
//
// Status transitions are driven by the Check/Install goroutines in
// handler/agent_server.go:
//
//	unknown → checking → ready | error
//	unknown → installing → ready | error
type AgentServer struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Host          string  `json:"host"`
	Port          int     `json:"port"`
	Username      string  `json:"username"`
	AuthType      string  `json:"auth_type"`
	AuthValue     string  `json:"-"`               // ciphertext, never in API responses
	AuthValueSet  bool    `json:"auth_value_set"`  // true when a credential is configured
	AuthValueAlgo string  `json:"auth_value_algo"` // algorithm id (currently "aes-gcm")
	Status        string  `json:"status"`
	LastCheckAt   *string `json:"last_check_at"`
	CheckResult   string  `json:"check_result"`
	// InstallJobID is the JobStore job id of the running install on this server,
	// or "" when no install is in flight. Persisted so the frontend can
	// reconnect to the SSE stream after a page refresh — without this the
	// component state jobId is lost on reload and the install keeps running
	// silently until Finish. Cleared by runInstall's defer on Finish so a
	// stale id never lingers after the job has actually gone.
	InstallJobID string `json:"install_job_id"`
	// WorkerVersion is the nova-agent-worker version the last successful install
	// stamped into the uploaded server.mjs (mirrors handler.agentWorkerVersion).
	// The check flow compares the running worker's reported version against this
	// to detect a stale process that survived a restart.
	WorkerVersion string `json:"worker_version"`
	// ClaudeBin / NodeBin are the absolute paths the install flow resolved for
	// the 'claude' and 'node' binaries (mirrors the on-disk
	// ~/.novaworkbench/{extra-paths,node-bin}). Empty until the first install
	// captures them; once captured they survive across Check / startWorkerIfDown
	// runs unless a re-install overwrites them. Surfaced in the settings UI
	// so operators can audit "where is claude actually coming from" without
	// SSH-ing back to the agent host.
	ClaudeBin string `json:"claude_bin"`
	NodeBin   string `json:"node_bin"`
	// ExtraPaths is a newline-separated list of PATH dirs the install flow
	// captured into ~/.novaworkbench/extra-paths (root's npm-global/bin,
	// nvm's ~/.nvm/versions/node/*/bin, brew's /opt/homebrew/bin, ...). The
	// worker process still reads the file directly (so this is a mirror for
	// UI display + future per-server env injection); we keep the column as
	// plain TEXT rather than JSON-encoded to match the file's own format.
	ExtraPaths string `json:"extra_paths"`
	// SystemInfo is a JSON-encoded snapshot of OS / kernel / hostname / CPUs /
	// memory / disk usage / IPs / uptime / claude_version, captured at the
	// end of runCheck (and once after runInstall). Empty string until the
	// first successful collection. UI parses on the fly into the asset
	// details panel.
	SystemInfo string `json:"system_info"`
	// SystemInfoAt is the timestamp of the last system_info collection, or
	// nil when never collected. UI uses it to render "last inventory N min ago".
	SystemInfoAt *string `json:"system_info_collected_at"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// CreateAgentServerReq is the create payload. AuthValue is the plaintext
// credential as typed by the user; the service layer encrypts it before
// storing.
type CreateAgentServerReq struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Username  string `json:"username"`
	AuthType  string `json:"auth_type"`
	AuthValue string `json:"auth_value"`
}

// UpdateAgentServerReq is the patch payload. nil pointer = "do not change",
// matching the convention used elsewhere in this package (requirement.go).
// To remove a credential, send AuthValue = "" (empty string) explicitly —
// callers should set ClearCredential=true via AuthValue=="" and ensure the
// service treats empty as "wipe" when AuthValue pointer is non-nil.
type UpdateAgentServerReq struct {
	Name      *string `json:"name"`
	Host      *string `json:"host"`
	Port      *int    `json:"port"`
	Username  *string `json:"username"`
	AuthType  *string `json:"auth_type"`
	AuthValue *string `json:"auth_value"` // nil = unchanged; pointer to "" = clear
}

// AgentServer constants (status values).
const (
	AgentServerStatusUnknown    = "unknown"
	AgentServerStatusChecking   = "checking"
	AgentServerStatusInstalling = "installing"
	AgentServerStatusReady      = "ready"
	AgentServerStatusError      = "error"

	AgentServerAuthKey      = "key"
	AgentServerAuthPassword = "password"

	AgentServerAuthAlgoAESGCM = "aes-gcm"
)
