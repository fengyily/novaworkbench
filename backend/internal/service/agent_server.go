package service

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/secret"
	"github.com/novaworkbench/backend/internal/util"
)

// AgentServerService owns CRUD + status transitions for the agent_servers
// table. The credential (auth_value) is encrypted at rest via internal/secret
// before INSERT/UPDATE, and decrypted only inside GetWithCredential — every
// other accessor returns the ciphertext with AuthValueSet=true so the API
// can distinguish "no credential configured" from "credential configured but
// hidden" without ever leaking the plaintext.
type AgentServerService struct {
	db *db.DB
}

func NewAgentServerService(d *db.DB) *AgentServerService {
	return &AgentServerService{db: d}
}

// scanRow reads one row into a model.AgentServer. AuthValue is left as the
// raw ciphertext; the public List/Get never decrypts.
func scanRow(row interface {
	Scan(...any) error
}) (*model.AgentServer, error) {
	var a model.AgentServer
	var authValue sql.NullString
	var lastCheck sql.NullString
	err := row.Scan(
		&a.ID, &a.Name, &a.Host, &a.Port, &a.Username,
		&a.AuthType, &authValue, &a.AuthValueAlgo,
		&a.Status, &lastCheck, &a.CheckResult,
		&a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if authValue.Valid && authValue.String != "" {
		a.AuthValue = authValue.String
		a.AuthValueSet = true
	}
	if lastCheck.Valid && lastCheck.String != "" {
		ts := lastCheck.String
		a.LastCheckAt = &ts
	}
	return &a, nil
}

const selectColumns = `id, name, host, port, username, auth_type, auth_value, auth_value_algo, status, last_check_at, check_result, created_at, updated_at`

// List returns all servers ordered by creation time. Credentials are NOT
// decrypted — callers must use GetWithCredential for the plaintext.
func (s *AgentServerService) List() ([]*model.AgentServer, error) {
	rows, err := s.db.Query(`SELECT ` + selectColumns + ` FROM agent_servers ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.AgentServer
	for rows.Next() {
		a, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Get fetches one row by id. auth_value is the raw ciphertext.
func (s *AgentServerService) Get(id string) (*model.AgentServer, error) {
	row := s.db.QueryRow(`SELECT `+selectColumns+` FROM agent_servers WHERE id = ?`, id)
	a, err := scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("agent server %s not found", id)
	}
	return a, err
}

// GetWithCredential returns the row plus the decrypted plaintext credential.
// The plaintext must be held only in memory and used immediately for an SSH
// connection; do not store or log it.
func (s *AgentServerService) GetWithCredential(id string) (*model.AgentServer, string, error) {
	a, err := s.Get(id)
	if err != nil {
		return nil, "", err
	}
	if a.AuthValue == "" {
		return a, "", nil
	}
	plain, err := secret.Decrypt(a.AuthValue)
	if err != nil {
		return nil, "", fmt.Errorf("agent server %s credential: %w (master key changed?)", id, err)
	}
	return a, plain, nil
}

// Create inserts a new row. The plaintext AuthValue is encrypted before
// INSERT; an empty AuthValue is stored as the empty string (AuthValueSet=false).
func (s *AgentServerService) Create(req model.CreateAgentServerReq) (*model.AgentServer, error) {
	if err := validateCreate(req); err != nil {
		return nil, err
	}
	ciphertext, err := encryptCredential(req.AuthValue)
	if err != nil {
		return nil, err
	}

	id := util.NewID("agent")
	now := time.Now()
	port := req.Port
	if port == 0 {
		port = 22
	}

	_, err = s.db.Exec(
		`INSERT INTO agent_servers
		 (id, name, host, port, username, auth_type, auth_value, auth_value_algo, status, check_result, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
		id, req.Name, req.Host, port, req.Username,
		req.AuthType, ciphertext, model.AgentServerAuthAlgoAESGCM,
		model.AgentServerStatusUnknown, now, now,
	)
	if err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Update applies a patch. nil pointer fields are skipped. AuthValue is
// special: a non-nil pointer with empty string clears the stored credential
// (used by the "rotate" UI when the user wants to remove the key); a nil
// pointer leaves the existing credential alone.
func (s *AgentServerService) Update(id string, req model.UpdateAgentServerReq) (*model.AgentServer, error) {
	existing, err := s.Get(id)
	if err != nil {
		return nil, err
	}

	if req.Name != nil {
		existing.Name = *req.Name
	}
	if req.Host != nil {
		existing.Host = *req.Host
	}
	if req.Port != nil {
		existing.Port = *req.Port
		if existing.Port == 0 {
			existing.Port = 22
		}
	}
	if req.Username != nil {
		existing.Username = *req.Username
	}
	if req.AuthType != nil {
		if err := validateAuthType(*req.AuthType); err != nil {
			return nil, err
		}
		existing.AuthType = *req.AuthType
	}

	// Auth value rotation:
	//   pointer nil      → keep existing
	//   pointer to ""    → wipe credential (rare; usually UI re-uploads)
	//   pointer to "..." → encrypt and replace
	newAuthSet := false
	newCipher := existing.AuthValue
	if req.AuthValue != nil {
		if *req.AuthValue == "" {
			newCipher = ""
		} else {
			cipher, err := secret.Encrypt(*req.AuthValue)
			if err != nil {
				return nil, err
			}
			newCipher = cipher
		}
		newAuthSet = true
	}

	now := time.Now()
	_, err = s.db.Exec(
		`UPDATE agent_servers
		 SET name = ?, host = ?, port = ?, username = ?, auth_type = ?,
		     auth_value = ?, auth_value_algo = ?, updated_at = ?
		 WHERE id = ?`,
		existing.Name, existing.Host, existing.Port, existing.Username,
		existing.AuthType, newCipher, model.AgentServerAuthAlgoAESGCM,
		now, id,
	)
	if err != nil {
		return nil, err
	}
	_ = newAuthSet // (could be used to invalidate cached credentials)
	return s.Get(id)
}

// Delete removes a server. Caller is responsible for surfacing any in-flight
// background job to the user; this just removes the row.
func (s *AgentServerService) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM agent_servers WHERE id = ?`, id)
	return err
}

// UpdateStatus is the Check/Install goroutine's write-back path. lastCheckAt
// is always updated to now, even when status="error", so the UI's "上次检查"
// label reflects reality.
func (s *AgentServerService) UpdateStatus(id, status, checkResult string) error {
	if !validStatus(status) {
		return fmt.Errorf("agent server: invalid status %q", status)
	}
	_, err := s.db.Exec(
		`UPDATE agent_servers
		 SET status = ?, check_result = ?, last_check_at = ?, updated_at = ?
		 WHERE id = ?`,
		status, checkResult, time.Now(), time.Now(), id,
	)
	return err
}

// --- helpers ---

func encryptCredential(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	return secret.Encrypt(plain)
}

func validateCreate(req model.CreateAgentServerReq) error {
	if req.Name == "" {
		return errors.New("name is required")
	}
	if req.Host == "" {
		return errors.New("host is required")
	}
	if req.Username == "" {
		return errors.New("username is required")
	}
	return validateAuthType(req.AuthType)
}

func validateAuthType(t string) error {
	if t == "" {
		return nil // defaults to "key" at the model layer
	}
	if t != model.AgentServerAuthKey && t != model.AgentServerAuthPassword {
		return fmt.Errorf("auth_type must be %q or %q", model.AgentServerAuthKey, model.AgentServerAuthPassword)
	}
	return nil
}

func validStatus(s string) bool {
	switch s {
	case model.AgentServerStatusUnknown,
		model.AgentServerStatusChecking,
		model.AgentServerStatusInstalling,
		model.AgentServerStatusReady,
		model.AgentServerStatusError:
		return true
	}
	return false
}
