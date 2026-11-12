package handler

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/util"
)

// claudeTranscriptProbe summarises what the Claude CLI recorded for ONE
// session during a bounded time window.
//
// It exists to answer a question that the process-level diagnostics cannot:
// "did the request ever leave the machine?" A run can die with an empty
// stderr, one stream event and an exit code we manufactured ourselves — and
// without the transcript you cannot tell "the model was called and the reply
// was lost" from "the CLI died before the first HTTP request was made".
//
// PRIVACY: this probe reads record *types* and *timestamps* only. It never
// returns, logs or stores record content — the assistant text, the user
// prompt and the tool payloads all stay on disk. Only counts and the latest
// timestamp leave this file.
type claudeTranscriptProbe struct {
	// Found is true when the transcript file exists and could be opened.
	Found bool
	// TotalRecords counts the records written inside the window.
	TotalRecords int
	// APIRequests counts "api-request" records in the window — the hard
	// evidence that the CLI actually called the model.
	APIRequests int
	// Assistants counts "assistant" records in the window.
	Assistants int
	// LastActivity is the timestamp of the newest record in the window.
	LastActivity time.Time
	// Path is the transcript file that was inspected (for error messages).
	Path string
}

// RequestLeftMachine reports whether the CLI demonstrably reached the model
// during the window. False with TotalRecords > 0 means the CLI was alive and
// writing, but never issued a request — the classic "fork died before the
// first API call" signature.
func (p claudeTranscriptProbe) RequestLeftMachine() bool {
	return p.APIRequests > 0
}

// Describe renders a short, content-free summary suitable for a job log line.
// It answers the only question that matters when a run dies early: did the
// request ever leave the machine?
func (p claudeTranscriptProbe) Describe() string {
	if !p.Found {
		return "会话记录=未找到（CLI 未落盘任何本次运行的记录）"
	}
	if !p.RequestLeftMachine() {
		return "会话记录=本次运行未发出任何模型请求（记录 " + strconv.Itoa(p.TotalRecords) +
			" 条 / api-request=0 / assistant=0）"
	}
	return "会话记录=本次运行已发出 " + strconv.Itoa(p.APIRequests) +
		" 次模型请求（记录 " + strconv.Itoa(p.TotalRecords) + " 条）"
}

// appendTranscriptEvidence appends the content-free transcript summary to an
// error message, so the user can tell "the request never left the machine"
// from "the request was made but no answer came back".
func appendTranscriptEvidence(msg string, p claudeTranscriptProbe) string {
	if msg == "" {
		return msg
	}
	return msg + "\n\n" + p.Describe()
}

// claudeTranscriptPath computes where the CLI stores the transcript of a
// session that ran in workDir. The layout is
// <claude home>/projects/<EncodeClaudeSlug(workDir)>/<sid>.jsonl.
func claudeTranscriptPath(workDir, sid string) string {
	if workDir == "" || sid == "" {
		return ""
	}
	return filepath.Join(claudeSessionHome(), "projects", util.EncodeClaudeSlug(workDir), sid+".jsonl")
}

// probeClaudeTranscript inspects the session transcript of (workDir, sid) and
// counts only the records written at or after `since`.
//
// The `since` filter is essential: a forked session inherits the parent's
// whole history into its own JSONL, so "does the file contain an api-request
// record?" would answer about the PARENT's traffic, not this run's.
func probeClaudeTranscript(workDir, sid string, since time.Time) claudeTranscriptProbe {
	res := claudeTranscriptProbe{Path: claudeTranscriptPath(workDir, sid)}
	if res.Path == "" {
		return res
	}
	f, err := os.Open(res.Path)
	if err != nil {
		return res
	}
	defer f.Close()
	res.Found = true

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 256*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// We only need the record's type and timestamp. Parsing the whole
		// record into a map is wasteful for multi-MB lines, so decode into a
		// struct with only the two fields we care about. Unknown fields are
		// ignored by encoding/json — and the CONTENT is never read into the
		// struct at all, so it cannot leak.
		var head struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal([]byte(line), &head); err != nil {
			continue
		}
		ts, ok := parseTranscriptTimestamp(head.Timestamp)
		if ok && !since.IsZero() && ts.Before(since) {
			// Inherited from the parent session — not this run.
			continue
		}
		res.TotalRecords++
		switch head.Type {
		case "api-request":
			res.APIRequests++
		case "assistant":
			res.Assistants++
		}
		if ok && ts.After(res.LastActivity) {
			res.LastActivity = ts
		}
	}
	return res
}

// parseTranscriptTimestamp parses the RFC3339 timestamp the CLI writes on
// every transcript record.
func parseTranscriptTimestamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		// Some records use a slightly different layout; try the plain
		// RFC3339 form before giving up.
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, false
		}
	}
	return t, true
}
