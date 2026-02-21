package refinery

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Common errors
var (
	ErrNotRunning     = errors.New("refinery not running")
	ErrAlreadyRunning = errors.New("refinery already running")
	ErrNoQueue        = errors.New("no items in queue")
)

// Manager handles refinery lifecycle and queue operations.
type Manager struct {
	rig     *rig.Rig
	workDir string
	output  io.Writer // Output destination for user-facing messages
}

// NewManager creates a new refinery manager for a rig.
func NewManager(r *rig.Rig) *Manager {
	return &Manager{
		rig:     r,
		workDir: r.Path,
		output:  os.Stdout,
	}
}

// SetOutput sets the output writer for user-facing messages.
// This is useful for testing or redirecting output.
func (m *Manager) SetOutput(w io.Writer) {
	m.output = w
}

// SessionName returns the tmux session name for this refinery.
func (m *Manager) SessionName() string {
	return session.RefinerySessionName(m.rig.Name)
}

// IsRunning checks if the refinery Go daemon is running.
// Primary check: Go daemon PID file.
// Fallback check: legacy tmux session (transition period).
func (m *Manager) IsRunning() (bool, error) {
	// Check Go daemon PID file first
	running, _, err := IsRefineryDaemonRunning(m.rig.Path)
	if err != nil {
		return false, err
	}
	if running {
		return true, nil
	}

	// Fallback: check for legacy tmux session
	t := tmux.NewTmux()
	sessionName := m.SessionName()
	status := t.CheckSessionHealth(sessionName, 0)
	return status == tmux.SessionHealthy, nil
}

// IsHealthy checks if the refinery is running and has been active recently.
// Unlike IsRunning which only checks process liveness, this also detects hung
// sessions where Claude is alive but hasn't produced output in maxInactivity.
// Returns the detailed ZombieStatus for callers that need to distinguish
// between different failure modes.
func (m *Manager) IsHealthy(maxInactivity time.Duration) tmux.ZombieStatus {
	t := tmux.NewTmux()
	return t.CheckSessionHealth(m.SessionName(), maxInactivity)
}

// Status returns information about the refinery.
// Returns nil session info for the Go daemon (no tmux session).
func (m *Manager) Status() (*tmux.SessionInfo, error) {
	// Check Go daemon
	running, _, err := IsRefineryDaemonRunning(m.rig.Path)
	if err != nil {
		return nil, fmt.Errorf("checking daemon: %w", err)
	}
	if running {
		return nil, nil // Go daemon running but no SessionInfo
	}

	// Fallback: check legacy tmux session
	t := tmux.NewTmux()
	sessionID := m.SessionName()
	hasSession, err := t.HasSession(sessionID)
	if err != nil {
		return nil, fmt.Errorf("checking session: %w", err)
	}
	if !hasSession {
		return nil, ErrNotRunning
	}
	return t.GetSessionInfo(sessionID)
}

// Start starts the refinery Go daemon as a background subprocess.
// The agentOverride parameter is accepted for API compatibility but ignored
// (the Go daemon has no LLM agent to override).
func (m *Manager) Start(_ bool, _ string) error {
	// Check if Go daemon already running
	running, _, err := IsRefineryDaemonRunning(m.rig.Path)
	if err != nil {
		return fmt.Errorf("checking daemon: %w", err)
	}
	if running {
		return ErrAlreadyRunning
	}

	// Kill any leftover legacy tmux session (transition cleanup)
	t := tmux.NewTmux()
	sessionID := m.SessionName()
	if hasSession, _ := t.HasSession(sessionID); hasSession {
		_, _ = fmt.Fprintln(m.output, "⚠ Cleaning up legacy refinery tmux session...")
		_ = t.KillSession(sessionID)
	}

	// Ensure PID directory exists
	pidDir := filepath.Dir(RefineryDaemonPidFile(m.rig.Path))
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		return fmt.Errorf("creating refinery directory: %w", err)
	}

	// Find the gt binary (use current executable for reliability)
	gtBin, err := os.Executable()
	if err != nil {
		// Fallback to PATH lookup
		gtBin, err = exec.LookPath("gt")
		if err != nil {
			gtBin = "gt"
		}
	}

	// Determine working directory for the daemon
	workDir := m.rig.Path
	refineryRigDir := filepath.Join(m.rig.Path, "refinery", "rig")
	if _, err := os.Stat(refineryRigDir); err == nil {
		workDir = refineryRigDir
	} else if mayorRigDir := filepath.Join(m.rig.Path, "mayor", "rig"); func() bool {
		_, e := os.Stat(mayorRigDir)
		return e == nil
	}() {
		workDir = mayorRigDir
	}

	// Launch: gt refinery daemon <rig-name>
	cmd := exec.Command(gtBin, "refinery", "daemon", m.rig.Name) //nolint:gosec // G204: args built internally
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"GT_REFINERY=1",
		fmt.Sprintf("GT_RIG=%s", m.rig.Name),
		fmt.Sprintf("GT_ROLE=refinery"),
	)
	// Redirect output to log file
	logPath := filepath.Join(m.rig.Path, "refinery", "refinery.log")
	logFile, logErr := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if logErr == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	// Detach from process group so daemon survives parent exit
	setSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return fmt.Errorf("starting refinery daemon: %w", err)
	}

	if logFile != nil {
		_ = logFile.Close()
	}

	// Detach: don't Wait() — daemon runs independently
	go func() { _ = cmd.Wait() }()

	_, _ = fmt.Fprintf(m.output, "Refinery Go daemon started (PID %d)\n", cmd.Process.Pid)
	return nil
}

// Stop stops the refinery daemon.
func (m *Manager) Stop() error {
	// Try to stop Go daemon first
	running, pid, err := IsRefineryDaemonRunning(m.rig.Path)
	if err != nil {
		return fmt.Errorf("checking daemon: %w", err)
	}
	if running && pid > 0 {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return fmt.Errorf("finding process %d: %w", pid, err)
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			return fmt.Errorf("stopping daemon (PID %d): %w", pid, err)
		}
		return nil
	}

	// Fallback: stop legacy tmux session
	t := tmux.NewTmux()
	sessionID := m.SessionName()
	hasSession, _ := t.HasSession(sessionID)
	if !hasSession {
		return ErrNotRunning
	}
	return t.KillSession(sessionID)
}

// Queue returns the current merge queue.
// Uses beads merge-request issues as the source of truth (not git branches).
// ZFC-compliant: beads is the source of truth, no state file.
func (m *Manager) Queue() ([]QueueItem, error) {
	// Query beads for open merge-request issues
	// BeadsPath() returns the git-synced beads location
	b := beads.New(m.rig.BeadsPath())
	issues, err := b.List(beads.ListOptions{
		Label:    "gt:merge-request",
		Status:   "open",
		Priority: -1, // No priority filter
	})
	if err != nil {
		return nil, fmt.Errorf("querying merge queue from beads: %w", err)
	}

	// Score and sort issues by priority score (highest first)
	now := time.Now()
	type scoredIssue struct {
		issue *beads.Issue
		score float64
	}
	scored := make([]scoredIssue, 0, len(issues))
	for _, issue := range issues {
		// Defensive filter: bd status filters can drift; queue must only include open MRs.
		if issue == nil || issue.Status != "open" {
			continue
		}
		score := m.calculateIssueScore(issue, now)
		scored = append(scored, scoredIssue{issue: issue, score: score})
	}

	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	// Convert scored issues to queue items
	var items []QueueItem
	pos := 1
	for _, s := range scored {
		mr := m.issueToMR(s.issue)
		if mr != nil {
			items = append(items, QueueItem{
				Position: pos,
				MR:       mr,
				Age:      formatAge(mr.CreatedAt),
			})
			pos++
		}
	}

	return items, nil
}

// calculateIssueScore computes the priority score for an MR issue.
// Higher scores mean higher priority (process first).
func (m *Manager) calculateIssueScore(issue *beads.Issue, now time.Time) float64 {
	fields := beads.ParseMRFields(issue)

	// Parse MR creation time
	mrCreatedAt := parseTime(issue.CreatedAt)
	if mrCreatedAt.IsZero() {
		mrCreatedAt = now // Fallback
	}

	// Build score input
	input := ScoreInput{
		Priority:    issue.Priority,
		MRCreatedAt: mrCreatedAt,
		Now:         now,
	}

	// Add fields from MR metadata if available
	if fields != nil {
		input.RetryCount = fields.RetryCount

		// Parse convoy created at if available
		if fields.ConvoyCreatedAt != "" {
			if convoyTime := parseTime(fields.ConvoyCreatedAt); !convoyTime.IsZero() {
				input.ConvoyCreatedAt = &convoyTime
			}
		}
	}

	return ScoreMRWithDefaults(input)
}

// issueToMR converts a beads issue to a MergeRequest.
func (m *Manager) issueToMR(issue *beads.Issue) *MergeRequest {
	if issue == nil {
		return nil
	}

	// Get configured default branch for this rig
	defaultBranch := m.rig.DefaultBranch()

	fields := beads.ParseMRFields(issue)
	if fields == nil {
		// No MR fields in description, construct from title/ID
		return &MergeRequest{
			ID:           issue.ID,
			IssueID:      issue.ID,
			Status:       MROpen,
			CreatedAt:    parseTime(issue.CreatedAt),
			TargetBranch: defaultBranch,
		}
	}

	// Default target to rig's default branch if not specified
	target := fields.Target
	if target == "" {
		target = defaultBranch
	}

	return &MergeRequest{
		ID:           issue.ID,
		Branch:       fields.Branch,
		Worker:       fields.Worker,
		IssueID:      fields.SourceIssue,
		TargetBranch: target,
		Status:       MROpen,
		CreatedAt:    parseTime(issue.CreatedAt),
	}
}

// parseTime parses a time string, returning zero time on error.
func parseTime(s string) time.Time {
	// Try RFC3339 first (most common)
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		// Try date-only format as fallback
		t, _ = time.Parse("2006-01-02", s)
	}
	return t
}

// formatAge formats a duration since the given time.
func formatAge(t time.Time) string {
	d := time.Since(t)

	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// Common errors for MR operations
var (
	ErrMRNotFound  = errors.New("merge request not found")
	ErrMRNotFailed = errors.New("merge request has not failed")
)

// GetMR returns a merge request by ID.
// ZFC-compliant: delegates to FindMR which uses beads as source of truth.
// Deprecated: Use FindMR directly for more flexible matching.
func (m *Manager) GetMR(id string) (*MergeRequest, error) {
	return m.FindMR(id)
}

// FindMR finds a merge request by ID or branch name in the queue.
func (m *Manager) FindMR(idOrBranch string) (*MergeRequest, error) {
	queue, err := m.Queue()
	if err != nil {
		return nil, err
	}

	for _, item := range queue {
		// Match by ID
		if item.MR.ID == idOrBranch {
			return item.MR, nil
		}
		// Match by branch name (with or without polecat/ prefix)
		if item.MR.Branch == idOrBranch {
			return item.MR, nil
		}
		if constants.BranchPolecatPrefix+idOrBranch == item.MR.Branch {
			return item.MR, nil
		}
		// Match by worker name (partial match for convenience)
		if strings.Contains(item.MR.ID, idOrBranch) {
			return item.MR, nil
		}
	}

	return nil, ErrMRNotFound
}

// Retry is deprecated - the Refinery agent handles retry logic autonomously.
// ZFC-compliant: no state file, agent uses beads issue status.
// The agent will automatically retry failed MRs in its patrol cycle.
func (m *Manager) Retry(_ string, _ bool) error {
	_, _ = fmt.Fprintln(m.output, "Note: Retry is deprecated. The Refinery agent handles retries autonomously via beads.")
	return nil
}

// RegisterMR is deprecated - MRs are registered via beads merge-request issues.
// ZFC-compliant: beads is the source of truth, not state file.
// Use 'gt mr create' or create a merge-request type bead directly.
func (m *Manager) RegisterMR(_ *MergeRequest) error {
	return fmt.Errorf("RegisterMR is deprecated: use beads to create merge-request issues")
}

// RejectMR manually rejects a merge request.
// It closes the MR with rejected status and optionally notifies the worker.
// Returns the rejected MR for display purposes.
func (m *Manager) RejectMR(idOrBranch string, reason string, notify bool) (*MergeRequest, error) {
	mr, err := m.FindMR(idOrBranch)
	if err != nil {
		return nil, err
	}

	// Verify MR is open or in_progress (can't reject already closed)
	if mr.IsClosed() {
		return nil, fmt.Errorf("%w: MR is already closed with reason: %s", ErrClosedImmutable, mr.CloseReason)
	}

	// Close the bead in storage with the rejection reason
	b := beads.New(m.rig.BeadsPath())
	if err := b.CloseWithReason("rejected: "+reason, mr.ID); err != nil {
		return nil, fmt.Errorf("failed to close MR bead: %w", err)
	}

	// Update in-memory state for return value
	if err := mr.Close(CloseReasonRejected); err != nil {
		// Non-fatal: bead is already closed, just log
		_, _ = fmt.Fprintf(m.output, "Warning: failed to update MR state: %v\n", err)
	}
	mr.Error = reason

	// Optionally notify worker
	if notify {
		m.notifyWorkerRejected(mr, reason)
	}

	return mr, nil
}

// notifyWorkerRejected sends a rejection notification to a polecat.
func (m *Manager) notifyWorkerRejected(mr *MergeRequest, reason string) {
	router := mail.NewRouter(m.workDir)
	msg := &mail.Message{
		From:    fmt.Sprintf("%s/refinery", m.rig.Name),
		To:      fmt.Sprintf("%s/%s", m.rig.Name, mr.Worker),
		Subject: "Merge request rejected",
		Body: fmt.Sprintf(`Your merge request has been rejected.

Branch: %s
Issue: %s
Reason: %s

Please review the feedback and address the issues before resubmitting.`,
			mr.Branch, mr.IssueID, reason),
		Priority: mail.PriorityNormal,
	}
	if err := router.Send(msg); err != nil {
		log.Printf("warning: notifying worker of rejection for %s: %v", mr.IssueID, err)
	}
}

// findTownRoot walks up directories to find the town root.
func findTownRoot(startPath string) string {
	path := startPath
	for {
		// Check for mayor/ subdirectory (indicates town root)
		if _, err := os.Stat(filepath.Join(path, "mayor")); err == nil {
			return path
		}
		// Check for config.json with type: workspace
		configPath := filepath.Join(path, "config.json")
		if data, err := os.ReadFile(configPath); err == nil {
			if strings.Contains(string(data), `"type": "workspace"`) {
				return path
			}
		}

		parent := filepath.Dir(path)
		if parent == path {
			break // Reached root
		}
		path = parent
	}
	return ""
}
