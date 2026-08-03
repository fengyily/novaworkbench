package handler

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

type RunSession struct {
	Job    *store.Job
	Cmd    *exec.Cmd
	cancel context.CancelFunc
}

type RunnerHandler struct {
	projectSvc *service.ProjectService
	jobs       *store.JobStore
	db         *sql.DB
	mu         sync.Mutex
	sessions   map[string]*RunSession // keyed by project ID
}

func NewRunnerHandler(projectSvc *service.ProjectService, jobs *store.JobStore, db *sql.DB) *RunnerHandler {
	return &RunnerHandler{
		projectSvc: projectSvc,
		jobs:       jobs,
		db:         db,
		sessions:   make(map[string]*RunSession),
	}
}

// detectComposeCmd finds the compose file and available docker compose binary.
// Returns bin ("docker" or "docker-compose"), the args prefix, and the compose filename.
func detectComposeCmd(workDir string) (bin string, binArgs []string, composeFile string, err error) {
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		if _, statErr := os.Stat(filepath.Join(workDir, name)); statErr == nil {
			composeFile = name
			break
		}
	}
	if composeFile == "" {
		return "", nil, "", fmt.Errorf("未找到 docker-compose.yml 或 compose.yml，请确认项目目录中存在 compose 文件")
	}

	if _, lookErr := exec.LookPath("docker"); lookErr == nil {
		probe := exec.Command("docker", "compose", "version")
		probe.Env = os.Environ()
		if probe.Run() == nil {
			return "docker", []string{"compose"}, composeFile, nil
		}
	}
	if _, lookErr := exec.LookPath("docker-compose"); lookErr == nil {
		return "docker-compose", nil, composeFile, nil
	}
	return "", nil, "", fmt.Errorf("未找到 docker compose 或 docker-compose，请先安装 Docker")
}

// Start launches docker compose up for the project.
// POST /api/projects/{id}/run/start
func (h *RunnerHandler) Start(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")

	project, err := h.projectSvc.Get(projectID)
	if err != nil {
		writeError(w, 404, "PROJECT_NOT_FOUND", "项目不存在")
		return
	}

	h.mu.Lock()
	if sess, exists := h.sessions[projectID]; exists {
		_, status, _ := sess.Job.Snapshot()
		if status == store.JobRunning {
			h.mu.Unlock()
			writeError(w, 409, "ALREADY_RUNNING", "该项目已有运行中的 compose 会话")
			return
		}
		delete(h.sessions, projectID)
	}
	h.mu.Unlock()

	bin, binArgs, composeFile, err := detectComposeCmd(project.LocalPath)
	if err != nil {
		writeError(w, 422, "COMPOSE_NOT_FOUND", err.Error())
		return
	}

	h.upsertRunConfig(projectID, composeFile)

	job := h.jobs.Create(projectID)
	writeJSON(w, 200, map[string]string{"job_id": job.ID})

	go h.runCompose(job, projectID, project.LocalPath, bin, binArgs, composeFile)
}

func (h *RunnerHandler) runCompose(job *store.Job, projectID, workDir, bin string, binArgs []string, composeFile string) {
	args := append(binArgs, "-f", composeFile, "up", "--build")

	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = workDir
	cmd.Env = os.Environ()

	// Merge stdout+stderr into a single pipe
	pr, pw, pipeErr := os.Pipe()
	if pipeErr != nil {
		cancel()
		job.Append(store.LogLine{Type: "error", Content: "pipe 创建失败: " + pipeErr.Error()})
		job.Finish(1, store.JobError)
		return
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		cancel()
		job.Append(store.LogLine{Type: "error", Content: "启动失败: " + err.Error()})
		job.Finish(1, store.JobError)
		return
	}
	// Close write end so the scanner gets EOF when the process exits
	pw.Close()

	h.mu.Lock()
	h.sessions[projectID] = &RunSession{Job: job, Cmd: cmd, cancel: cancel}
	h.mu.Unlock()

	log.Printf("[runner] project %s job %s started: %s %v", projectID, job.ID, bin, args)

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		lineType := "message"
		if strings.Contains(strings.ToLower(line), "error") {
			lineType = "error"
		}
		job.Append(store.LogLine{Type: lineType, Content: line})
	}
	pr.Close()

	waitErr := cmd.Wait()
	cancel()

	h.mu.Lock()
	delete(h.sessions, projectID)
	h.mu.Unlock()

	exitCode := 0
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	// Exit code 130 = SIGINT (graceful stop requested by user)
	if exitCode == 0 || exitCode == 130 {
		job.Append(store.LogLine{Type: "done", Content: "已停止"})
		job.Finish(exitCode, store.JobDone)
	} else {
		job.Append(store.LogLine{Type: "error", Content: fmt.Sprintf("compose 退出 (exit %d)", exitCode)})
		job.Finish(exitCode, store.JobError)
	}
	log.Printf("[runner] project %s job %s finished (exit %d)", projectID, job.ID, exitCode)
}

// Stop sends SIGINT to the running compose process.
// POST /api/projects/{id}/run/stop
func (h *RunnerHandler) Stop(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")

	h.mu.Lock()
	sess, exists := h.sessions[projectID]
	h.mu.Unlock()

	if !exists {
		writeError(w, 404, "NOT_RUNNING", "该项目当前没有运行中的会话")
		return
	}

	_, status, _ := sess.Job.Snapshot()
	if status != store.JobRunning {
		writeError(w, 404, "NOT_RUNNING", "该项目当前没有运行中的会话")
		return
	}

	sess.Job.Append(store.LogLine{Type: "message", Content: "正在停止..."})

	if sess.Cmd.Process != nil {
		_ = sess.Cmd.Process.Signal(syscall.SIGINT)
	}

	// Force-kill fallback after 5 seconds
	go func() {
		time.Sleep(5 * time.Second)
		_, s, _ := sess.Job.Snapshot()
		if s == store.JobRunning {
			sess.cancel()
			if sess.Cmd.Process != nil {
				_ = sess.Cmd.Process.Kill()
			}
		}
	}()

	writeJSON(w, 200, map[string]string{"status": "stopping"})
}

// Status returns the current run session state for a project.
// GET /api/projects/{id}/run/status
func (h *RunnerHandler) Status(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")

	h.mu.Lock()
	sess, exists := h.sessions[projectID]
	h.mu.Unlock()

	composeFile := h.getRunConfigComposeFile(projectID)

	if !exists {
		writeJSON(w, 200, map[string]interface{}{
			"status":       "stopped",
			"job_id":       nil,
			"log":          []store.LogLine{},
			"started_at":   nil,
			"finished_at":  nil,
			"compose_file": composeFile,
		})
		return
	}

	logLines, status, exitCode := sess.Job.Snapshot()
	if len(logLines) > 200 {
		logLines = logLines[len(logLines)-200:]
	}

	writeJSON(w, 200, map[string]interface{}{
		"status":       status,
		"job_id":       sess.Job.ID,
		"exit_code":    exitCode,
		"log":          logLines,
		"started_at":   sess.Job.StartedAt,
		"finished_at":  sess.Job.FinishedAt,
		"compose_file": composeFile,
	})
}

func (h *RunnerHandler) upsertRunConfig(projectID, composeFile string) {
	var existingID string
	err := h.db.QueryRow(
		`SELECT id FROM project_run_configs WHERE project_id = ? AND is_default = 1`,
		projectID,
	).Scan(&existingID)

	if err == sql.ErrNoRows {
		id := util.NewID("rc")
		_, _ = h.db.Exec(
			`INSERT INTO project_run_configs (id, project_id, compose_file, is_default) VALUES (?, ?, ?, 1)`,
			id, projectID, composeFile,
		)
	} else if err == nil {
		_, _ = h.db.Exec(
			`UPDATE project_run_configs SET compose_file = ? WHERE id = ?`,
			composeFile, existingID,
		)
	}
}

func (h *RunnerHandler) getRunConfigComposeFile(projectID string) string {
	var cf string
	_ = h.db.QueryRow(
		`SELECT compose_file FROM project_run_configs WHERE project_id = ? AND is_default = 1`,
		projectID,
	).Scan(&cf)
	return cf
}
