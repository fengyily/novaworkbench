// Package service — AttachmentService handles per-requirement file
// uploads. Files live on local disk under ~/.novaworkbench/attachments/
// <requirement_id>/<att_id>__<sha_short>__<safe_name>; metadata lives in
// the requirement_attachments table. Three wizard stages (analyst /
// architect / developer) all read from the same set via BuildManifest,
// which produces a Markdown block ready for prompt injection.
package service

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/util"
)

// AttachmentRoot is the absolute directory under which every per-requirement
// attachments directory lives. Tests / packaging overrides may rewrite this
// via SetAttachmentRoot; production wires to ~/.novaworkbench/attachments/.
var attachmentRoot = defaultAttachmentRoot()

func defaultAttachmentRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".novaworkbench", "attachments")
}

// SetAttachmentRoot overrides the root directory. Test helper.
func SetAttachmentRoot(p string) { attachmentRoot = p }
func AttachmentRoot() string     { return attachmentRoot }

// MaxAttachmentSize caps a single file at 2 GB. The HTTP layer applies this
// against http.MaxBytesReader before parsing the form, so a request larger than
// this cap fails with 413 TOO_LARGE before any bytes reach the form parser or
// the on-disk temp file. The cap exists purely as a DoS guardrail — wizards
// may legitimately attach multi-hundred-MB log dumps, so the headroom needs to
// be generous, but an unbounded cap lets a single bad client exhaust disk.
const MaxAttachmentSize = 2 << 30

// AttachmentService is the attachment store for one owner kind. The table
// and the owner column are fixed at construction time so the exact same
// method bodies back both requirement attachments and code-review
// attachments — the two owner id spaces (`req_*` / `crv_*`) never collide,
// so they also share one on-disk root (attachmentRoot/<owner_id>/).
//
// table / ownerCol are package-level constants chosen by the constructors
// below, never caller input: the fmt.Sprintf'd SQL has no injection surface.
type AttachmentService struct {
	db       *db.DB
	table    string
	ownerCol string
}

// NewAttachmentService builds the requirement-scoped store (the original,
// and still the only one the wizard stages use).
func NewAttachmentService(d *db.DB) *AttachmentService {
	return &AttachmentService{db: d, table: "requirement_attachments", ownerCol: "requirement_id"}
}

// NewCodeReviewAttachmentService builds the review-scoped twin. Same
// behaviour, different table — the owner is a code_reviews row, so the
// files live under attachmentRoot/<crv_id>/ and are injected into the
// reviewer's system prompt by handler.attachManifestFor.
func NewCodeReviewAttachmentService(d *db.DB) *AttachmentService {
	return &AttachmentService{db: d, table: "code_review_attachments", ownerCol: "review_id"}
}

// ownerDir returns the per-owner directory (creating it on first use).
// Returns absolute path.
func (s *AttachmentService) ownerDir(ownerID string) (string, error) {
	dir := filepath.Join(attachmentRoot, ownerID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// selectCols is the shared projection; every scan below reads these in
// order into model.Attachment.
func (s *AttachmentService) selectCols() string {
	return fmt.Sprintf(`id, %s, filename, stored_path, mime_type,
		       size_bytes, sha256, description, manifest_text, uploaded_by, uploaded_at`, s.ownerCol)
}

// List returns every attachment row for a requirement, ordered by
// upload time. The wizard's BuildManifest iterates this directly.
func (s *AttachmentService) List(ownerID string) ([]model.Attachment, error) {
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT %s
		FROM %s
		WHERE %s = ?
		ORDER BY uploaded_at ASC, id ASC
	`, s.selectCols(), s.table, s.ownerCol), ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Attachment
	for rows.Next() {
		var a model.Attachment
		var uploadedAt time.Time
		if err := rows.Scan(&a.ID, &a.OwnerID, &a.Filename, &a.StoredPath,
			&a.MimeType, &a.SizeBytes, &a.SHA256, &a.Description,
			&a.ManifestText, &a.UploadedBy, &uploadedAt); err != nil {
			return nil, err
		}
		a.UploadedAt = uploadedAt
		out = append(out, a)
	}
	return out, rows.Err()
}

// Get fetches a single attachment row by id. Returns nil + sql.ErrNoRows if
// missing.
func (s *AttachmentService) Get(attID string) (*model.Attachment, error) {
	var a model.Attachment
	var uploadedAt time.Time
	err := s.db.QueryRow(fmt.Sprintf(`
		SELECT %s FROM %s WHERE id = ?
	`, s.selectCols(), s.table), attID).Scan(&a.ID, &a.OwnerID, &a.Filename, &a.StoredPath,
		&a.MimeType, &a.SizeBytes, &a.SHA256, &a.Description,
		&a.ManifestText, &a.UploadedBy, &uploadedAt)
	if err != nil {
		return nil, err
	}
	a.UploadedAt = uploadedAt
	return &a, nil
}

// Upload streams the upload through a SHA256 hasher onto disk, registers the
// metadata row, and returns the persisted model. If (requirement_id, sha256)
// already exists, the existing row + file are reused — no duplicate storage
// across re-uploads of the same bytes. Detected archives (filename suffix
// .zip / .tar*) get a manifest_text snapshot via Go stdlib so Claude can
// plan unzip commands without a separate listing round trip; tar formats
// fall back to "external tool needed (tar) — call `tar -tf <path>` first".
//
// Errors wrap os.PathError / sqlite errors; handler maps to writeError.
func (s *AttachmentService) Upload(ownerID string, in model.AttachmentUpload) (*model.Attachment, error) {
	if strings.TrimSpace(ownerID) == "" {
		return nil, errors.New("owner id required")
	}
	if strings.TrimSpace(in.Filename) == "" {
		return nil, errors.New("filename required")
	}
	if in.Reader == nil {
		return nil, errors.New("file content required")
	}
	dir, err := s.ownerDir(ownerID)
	if err != nil {
		return nil, err
	}

	attID := util.NewID("att")
	safeName := sanitizeFilename(in.Filename)
	storedRel := "" // filled after we know the sha short
	tmp, err := os.CreateTemp(dir, "upload-*-"+safeName)
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer func() {
		// Best-effort cleanup if anything below fails before rename.
		_ = os.Remove(tmpPath)
	}()

	h := sha256.New()
	mw := io.MultiWriter(tmp, h)
	// Defense in depth: http.MaxBytesReader caps the request body before
	// ParseMultipartForm. Here we still stop at MaxAttachmentSize+1 byte so
	// a malformed caller bypassing the HTTP cap cannot exhaust disk.
	written, err := io.Copy(mw, io.LimitReader(in.Reader, MaxAttachmentSize+1))
	if err != nil {
		tmp.Close()
		return nil, fmt.Errorf("write upload: %w", err)
	}
	if written > MaxAttachmentSize {
		tmp.Close()
		return nil, fmt.Errorf("file exceeds %s cap", formatSize(MaxAttachmentSize))
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	sum := hex.EncodeToString(h.Sum(nil))
	short := sum[:8]

	// Reuse-on-same-hash. Caller re-uploading identical bytes returns the
	// original row + file path untouched; the (requirement_id, sha256)
	// unique index on the table backs this. Also refreshes description so
	// an "update the annotation only" intent still goes through Upload.
	var existing model.Attachment
	var upAt time.Time
	err = s.db.QueryRow(fmt.Sprintf(`
		SELECT %s FROM %s WHERE %s = ? AND sha256 = ?
	`, s.selectCols(), s.table, s.ownerCol), ownerID, sum).Scan(
		&existing.ID, &existing.OwnerID, &existing.Filename,
		&existing.StoredPath, &existing.MimeType, &existing.SizeBytes,
		&existing.SHA256, &existing.Description, &existing.ManifestText,
		&existing.UploadedBy, &upAt,
	)
	if err == nil {
		// Hash hit — update description (no-op if same) and return existing.
		if existing.Description != in.Description {
			if _, uerr := s.db.Exec(
				fmt.Sprintf(`UPDATE %s SET description = ? WHERE id = ?`, s.table),
				in.Description, existing.ID); uerr != nil {
				return nil, uerr
			}
			existing.Description = in.Description
		}
		return &existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// New row — move temp file to its permanent name.
	storedRel = filepath.ToSlash(filepath.Join(ownerID,
		fmt.Sprintf("%s__%s__%s", attID, short, safeName)))
	finalAbs := filepath.Join(attachmentRoot, filepath.FromSlash(storedRel))
	if err := os.Rename(tmpPath, finalAbs); err != nil {
		return nil, fmt.Errorf("rename upload: %w", err)
	}

	mime := sniffMime(finalAbs)
	manifest := buildArchiveManifest(finalAbs, in.Filename)

	now := time.Now().UTC()
	if _, err := s.db.Exec(fmt.Sprintf(`
		INSERT INTO %s
		  (id, %s, filename, stored_path, mime_type,
		   size_bytes, sha256, description, manifest_text, uploaded_by, uploaded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, s.table, s.ownerCol), attID, ownerID, in.Filename, storedRel, mime,
		written, sum, in.Description, manifest, in.UploadedBy, now); err != nil {
		// Roll back the renamed file if the DB write fails so we don't
		// leave orphan bytes on disk for a row that doesn't exist.
		_ = os.Remove(finalAbs)
		return nil, err
	}
	return &model.Attachment{
		ID: attID, OwnerID: ownerID, Filename: in.Filename,
		StoredPath: storedRel, MimeType: mime, SizeBytes: written,
		SHA256: sum, Description: in.Description, ManifestText: manifest,
		UploadedBy: in.UploadedBy, UploadedAt: now,
	}, nil
}

// UpdateDescription patches a single column. Returns the post-update row.
func (s *AttachmentService) UpdateDescription(attID, description string) (*model.Attachment, error) {
	if _, err := s.db.Exec(
		fmt.Sprintf(`UPDATE %s SET description = ? WHERE id = ?`, s.table),
		description, attID); err != nil {
		return nil, err
	}
	return s.Get(attID)
}

// Delete removes both the metadata row and its on-disk file. Idempotent —
// missing disk file is not an error. Returns the deleted row (mostly for
// logging; the handler can pick what to surface to the caller).
func (s *AttachmentService) Delete(attID string) error {
	var ownerID, storedPath string
	err := s.db.QueryRow(
		fmt.Sprintf(`SELECT %s, stored_path FROM %s WHERE id = ?`, s.ownerCol, s.table),
		attID).Scan(&ownerID, &storedPath)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(
		fmt.Sprintf(`DELETE FROM %s WHERE id = ?`, s.table), attID); err != nil {
		return err
	}
	abs := filepath.Join(attachmentRoot, filepath.FromSlash(storedPath))
	if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// AbsolutePath returns the on-disk absolute path for an attachment row.
// Handler uses this for downloads; wizard uses it for prompt rendering.
func (s *AttachmentService) AbsolutePath(a *model.Attachment) string {
	return filepath.Join(attachmentRoot, filepath.FromSlash(a.StoredPath))
}

// TotalSize returns the aggregate on-disk size of a requirement's
// attachments. Used by the upload handler as a per-requirement budget guard.
func (s *AttachmentService) TotalSize(ownerID string) (int64, error) {
	var total sql.NullInt64
	if err := s.db.QueryRow(
		fmt.Sprintf(`SELECT COALESCE(SUM(size_bytes), 0) FROM %s WHERE %s = ?`, s.table, s.ownerCol),
		ownerID).Scan(&total); err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// BuildManifest composes a Markdown block describing every attachment on a
// requirement, tailored for `--append-system-prompt` injection. Empty
// attachments return "" so callers can prepend unconditionally.
//
// Each entry renders as:
//
//	### <n>. <filename>  (<size>)
//	**用户描述**：<description>          (omitted when blank)
//	**位置**：`<attach_dir>/<filename>`  (always shown)
//	```
//	<manifest_text>                       (only for archives — empty otherwise)
//	```
//	_(用户未写描述)_                      (only when description is blank)
//
// Callers pass attachDir = the directory the LLM should treat as root (local
// exec → ~/.novaworkbench/attachments/<req_id>; remote → synced mirror on
// the agent server). The wizard injects both the manifest body AND a brief
// stage-specific intro line — see handler/wizard_common.go:InjectAttachments.
func (s *AttachmentService) BuildManifest(ownerID, attachDir string) string {
	atts, err := s.List(ownerID)
	if err != nil || len(atts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 📎 已附加文件\n\n")
	b.WriteString("下方的文件由用户手动上传，可通过 Read 工具按需读取；「描述」字段由用户为 AI 撰写，便于理解文件用途。\n\n")
	for i, a := range atts {
		fmt.Fprintf(&b, "### %d. %s  (%s)\n", i+1, a.Filename, formatSize(a.SizeBytes))
		if strings.TrimSpace(a.Description) != "" {
			fmt.Fprintf(&b, "**用户描述**：%s\n", a.Description)
		} else {
			b.WriteString("**用户描述**：_(用户未写描述,请根据文件名 / Read 后内容自行判断用途)_\n")
		}
		fmt.Fprintf(&b, "**位置**：`%s`\n", filepath.Join(attachDir, a.Filename))
		if strings.TrimSpace(a.ManifestText) != "" {
			fmt.Fprintf(&b, "\n```\n%s\n```\n", strings.TrimRight(a.ManifestText, "\n"))
		}
		// Image-type hint. Claude's Read tool natively renders common image
		// formats, so a pasted screenshot or design sketch is just as readable
		// as a text file. We surface this here so the LLM doesn't waste a turn
		// asking the user to describe the image, and so unusually large images
		// (which the client-side compressor should have caught but might have
		// slipped through) get a soft nudge.
		if strings.HasPrefix(a.MimeType, "image/") {
			b.WriteString("**类型**：图像 — Claude 可直接 `Read` 工具查看（API 原生支持 PNG/JPEG/GIF/WebP）\n")
			if a.SizeBytes > 2*1024*1024 {
				b.WriteString("**⚠ 文件 > 2MB，可能含未压缩原图，建议先 image-convert 工具降尺寸**\n")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// --- helpers ---

// sanitizeFilename strips path separators + control bytes so the file we
// store under a multi-segment name can't escape the per-requirement dir.
func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	if name == "." || name == "/" || name == "" {
		return "file"
	}
	name = strings.ReplaceAll(name, "..", "_")
	// Replace any remaining oddities (NUL, control chars).
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == 0, r < 32, r == 127:
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 120 {
		ext := filepath.Ext(out)
		stem := out[:len(out)-len(ext)]
		if len(stem) > 100 {
			stem = stem[:100]
		}
		out = stem + ext
	}
	return out
}

// formatSize renders a byte count as "256 B", "1.5 KB", "12.3 MB" or "1.4 GB".
// Public-facing only — we never use this in any audit / hash computation.
func formatSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
	}
}

// sniffMime returns a best-effort MIME based on the first 512 bytes, then
// falls back to the extension-derived type. We intentionally do NOT block
// any extension — every file type is accepted (logs, .pcap, dumps,
// binaries, archives alike).
func sniffMime(absPath string) string {
	f, err := os.Open(absPath)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	if ct := http.DetectContentType(buf[:n]); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// buildArchiveManifest returns a listing of entries inside common archive
// formats. zip is read in-process (Go stdlib); tar variants return a hint
// that the LLM must call `tar -tf <path>` first because reading tar
// headers requires sequential access — doing it here would mean streaming
// the whole archive again.
func buildArchiveManifest(absPath, origName string) string {
	lower := strings.ToLower(origName)
	if strings.HasSuffix(lower, ".zip") {
		return buildZipManifest(absPath)
	}
	if strings.HasSuffix(lower, ".tar") || strings.HasSuffix(lower, ".tar.gz") ||
		strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tar.bz2") ||
		strings.HasSuffix(lower, ".tbz2") || strings.HasSuffix(lower, ".tar.xz") ||
		strings.HasSuffix(lower, ".txz") {
		return "(此为 tar 归档,无法在服务端预览条目;请使用 Bash 调用 `tar -tf <path>` 列出)\n"
	}
	return ""
}

func buildZipManifest(absPath string) string {
	zr, err := zip.OpenReader(absPath)
	if err != nil {
		return fmt.Sprintf("(zip 解析失败:%v — Claude 仍可 Read 字节并尝试 Bash unzip)\n", err)
	}
	defer zr.Close()
	var b strings.Builder
	total := 0
	written := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		total++
		written += int(f.UncompressedSize64)
		fmt.Fprintf(&b, "%s  %s\n", formatSize(int64(f.UncompressedSize64)), f.Name)
	}
	fmt.Fprintf(&b, "── %d entries, %s uncompressed\n", total, formatSize(int64(written)))
	return b.String()
}

// ─── Agent-server sync ──────────────────────────────────────────────────────
//
// When a wizard stage runs against an Agent Server (remote SSH), Claude's
// `Read` tool cannot reach ~/.novaworkbench/attachments/<req_id>/ — the
// files need to be present on the remote host. SyncToAgentServer does an
// incremental SFTP copy keyed on (att_id, sha256); files whose sha hasn't
// changed since the last sync are skipped, so a follow-up coding run on
// the same requirement/server pair is essentially free (just the JSON
// state read + write).

// RemoteAttacher is the narrow SFTP surface AttachmentService needs.
// *ssh.Client satisfies this naturally (PutFile + Remove + Mkdirp),
// and tests can supply a stub by implementing the same three methods.
// Keeping the interface tiny also means callers can't accidentally
// depend on shell exec / port-forwarding they have no business with
// from this service. Remove is best-effort and IsNotExist-tolerant
// (the *ssh.Client wrapper already swallows os.ErrNotExist), so the
// orphan-cleanup loop never has to special-case "already gone".
type RemoteAttacher interface {
	PutFile(local, remote string, mode os.FileMode) error
	Mkdirp(remotePath string) error
	Remove(remotePath string) error
}

// AttachmentRootRemote is the canonical per-user directory on the Agent
// Server under which every per-requirement attachments directory lives.
// Mirrors AttachmentRoot on the local side, but lives outside any worktree
// so `git worktree remove` doesn't drop the synced files. Tests can
// override with SetAttachmentRootRemote.
var attachmentRootRemote = "~/.nova/attachments"

func AttachmentRootRemote() string { return attachmentRootRemote }

// remoteAttachDir returns the per-requirement directory on the remote host
// the wizard should treat as the attachment root (synced mirror). Always
// uses the tilde-prefixed path so the SFTP session expands $HOME on the
// remote side, matching the convention the ssh.Client.PutFile wrapper
// applies elsewhere.
func remoteAttachDir(reqID string) string {
	return filepath.ToSlash(filepath.Join(attachmentRootRemote, reqID))
}

// remoteBasenameFromLocal mirrors the local stored_path scheme — the
// filename on the remote host is the same <att_id>__<sha_short>__<safe_name>
// triple so deleting-or-reuploading a local row always maps to the
// equivalent file on the remote without an extra lookup table.
func remoteBasenameFromLocal(a *model.Attachment) string {
	return filepath.Base(filepath.FromSlash(a.StoredPath))
}

// syncStateEntry is one attachment's record in the per-server state
// JSON. SHA drives the dedup decision; Name is the basename on the
// remote (the upload loop also computes it but pinning it here means
// the orphan-cleanup pass can ssh.Remove without re-running the upload
// loop). Name == "" means "synced by an older version of this code
// before the field existed" — the entry is still respected for upload
// dedup but cannot be cleaned up remotely (no path to remove). Entries
// like that are populated with Name on the next successful re-upload.
type syncStateEntry struct {
	SHA  string `json:"sha"`
	Name string `json:"name"`
}

// syncStateShape is the JSON shape persisted on agent_servers.
//   {"<req_id>": {"<att_id>": {"sha": "...", "name": "..."}, ...}, ...}
type syncStateShape = map[string]map[string]syncStateEntry

// SyncToAgentServer reconciles the local attachment set for reqID with
// what the Agent Server has under ~/.nova/attachments/<req_id>/. Returns
// the canonical remote path the wizard should bake into the prompt so
// Claude's `Read` calls hit the right file on the remote host.
//
// Algorithm (one pass, three phases):
//
//  1. Read agent_servers.attachment_sync_state — a per-req map of
//     att_id → {sha, name}. A malformed cell falls back to a fresh
//     map (never silently drop a file).
//  2. Upload phase: walk local rows in upload order; for each, if the
//     remote's recorded sha differs (or the entry is missing), PutFile
//     the new bytes and refresh the entry's sha + name.
//  3. Orphan-prune phase: walk the previous state[req_id]; for any
//     att_id that no longer has a matching local row, ssh.Remove the
//     recorded remote name and drop the entry. Entries with Name==""
//     (legacy state) are left intact on remote because we don't know
//     what filename to remove — the eventual re-upload will repopulate
//     the name without us touching what's already there.
//
// The new state replaces state[req_id] entirely and is persisted in a
// single UPDATE; other reqs on the same server keep their state
// untouched.
func (s *AttachmentService) SyncToAgentServer(
	ctx context.Context,
	serverID string,
	remote RemoteAttacher,
	reqID string,
) (string, error) {
	if serverID == "" {
		return "", errors.New("serverID required")
	}
	if remote == nil {
		return "", errors.New("remote sftp handle required")
	}
	if strings.TrimSpace(reqID) == "" {
		return "", errors.New("requirementID required")
	}

	locals, err := s.List(reqID)
	if err != nil {
		return "", fmt.Errorf("list local attachments: %w", err)
	}

	stateJSON, err := s.loadSyncState(serverID)
	if err != nil {
		return "", fmt.Errorf("load sync state: %w", err)
	}
	state := parseSyncState(stateJSON)

	rd := remoteAttachDir(reqID)
	if err := remote.Mkdirp(rd); err != nil {
		return "", fmt.Errorf("mkdir remote %s: %w", rd, err)
	}

	// Snapshot the OLD state for this req so we can detect orphans
	// after the upload phase completes. The upload phase mutates
	// upgraded below; if the upload phase appends to upgraded instead
	// of replacing state[req_id], the orphan-prune phase can diff
	// cleanly by reference.
	previous := state[reqID]
	upgraded := map[string]syncStateEntry{}

	// Phase 2 — upload new / replaced files.
	for i := range locals {
		a := &locals[i]
		name := remoteBasenameFromLocal(a)
		upgraded[a.ID] = syncStateEntry{SHA: a.SHA256, Name: name}
		if prev, ok := previous[a.ID]; ok && prev.SHA == a.SHA256 {
			// sha unchanged — keep whatever remote filename was stored,
			// in case a future name-aware pruner benefits.
			upgraded[a.ID] = prev
			continue
		}
		localAbs := s.AbsolutePath(a)
		remotePath := filepath.ToSlash(filepath.Join(rd, name))
		if err := remote.PutFile(localAbs, remotePath, 0o644); err != nil {
			return "", fmt.Errorf("upload %s -> %s: %w", a.Filename, remotePath, err)
		}
	}

	// Phase 3 — orphan prune. Iterate the snapshot; any att_id that
	// didn't make it into `upgraded` is gone locally, so its remote
	// twin is safe to remove (skipping legacy entries with no name).
	if len(previous) > 0 {
		for attID, prev := range previous {
			if _, keep := upgraded[attID]; keep {
				continue
			}
			if prev.Name == "" {
				// Legacy entry — leave the remote file alone. The
				// next re-upload of the same att_id will repopulate
				// the name; until then the file stays as an orphan
				// the user can ignore (size bounded by MaxAttachmentSize
				// × #legacy entries).
				continue
			}
			remotePath := filepath.ToSlash(filepath.Join(rd, prev.Name))
			if err := remote.Remove(remotePath); err != nil {
				return "", fmt.Errorf("prune orphan %s: %w", remotePath, err)
			}
		}
	}

	state[reqID] = upgraded
	if err := s.saveSyncState(serverID, state); err != nil {
		return "", fmt.Errorf("save sync state: %w", err)
	}
	return rd, nil
}

// loadSyncState / saveSyncState: the JSON column on agent_servers. Reads
// are best-effort — corrupt JSON is replaced with {} so a future sync
// behaves like a fresh start (no worse than a schema migration).

func (s *AttachmentService) loadSyncState(serverID string) (string, error) {
	var raw string
	err := s.db.QueryRow(
		`SELECT attachment_sync_state FROM agent_servers WHERE id = ?`,
		serverID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "{}", nil
	}
	if err != nil {
		return "", err
	}
	if raw == "" {
		return "{}", nil
	}
	return raw, nil
}

func (s *AttachmentService) saveSyncState(serverID string, state syncStateShape) error {
	if state == nil {
		state = syncStateShape{}
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`UPDATE agent_servers SET attachment_sync_state = ?, updated_at = ? WHERE id = ?`,
		string(b), time.Now().UTC(), serverID)
	return err
}

// parseSyncState decodes the persisted JSON shape — silent fallback to
// a fresh map on any error. Backwards-compatible with the v0.5.x
// string-only entries: when a leaf is a bare string, it's treated as
// {SHA: the_string, Name: ""}. The Name="" entries route through the
// orphan-prune legacy-skip branch in SyncToAgentServer and become
// fully populated on their next re-upload.
func parseSyncState(raw string) syncStateShape {
	out := syncStateShape{}
	if strings.TrimSpace(raw) == "" || raw == "{}" {
		return out
	}
	var rawShape map[string]map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &rawShape); err != nil {
		return syncStateShape{}
	}
	if rawShape == nil {
		return out
	}
	for reqID, reqMap := range rawShape {
		if reqMap == nil {
			out[reqID] = map[string]syncStateEntry{}
			continue
		}
		normalized := map[string]syncStateEntry{}
		for attID, leaf := range reqMap {
			normalized[attID] = decodeSyncEntry(attID, leaf)
		}
		out[reqID] = normalized
	}
	return out
}

// decodeSyncEntry handles BOTH the legacy string-only shape (treated as
// {SHA: the_string, Name: ""}) and the current {"sha": ..., "name": ...}
// object. Returns zero-value on any decode error so a single corrupt
// entry can't poison the whole req's state.
func decodeSyncEntry(attID string, leaf json.RawMessage) syncStateEntry {
	// Try the object form first — it's the canonical shape.
	var obj syncStateEntry
	if err := json.Unmarshal(leaf, &obj); err == nil && (obj.SHA != "" || obj.Name != "") {
		return obj
	}
	// Fall back to the legacy string form: a bare sha256.
	var s string
	if err := json.Unmarshal(leaf, &s); err == nil && s != "" {
		return syncStateEntry{SHA: s}
	}
	return syncStateEntry{}
}
