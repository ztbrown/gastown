package swarm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/rig"
)

// Common errors
var (
	ErrSwarmNotFound  = errors.New("swarm not found")
	ErrSwarmExists    = errors.New("swarm already exists")
	ErrInvalidState   = errors.New("invalid state transition")
	ErrNoReadyTasks   = errors.New("no ready tasks")
	ErrBeadsNotFound  = errors.New("beads not available")
)

// Manager handles swarm lifecycle operations.
type Manager struct {
	rig     *rig.Rig
	swarms  map[string]*Swarm
	workDir string
}

// NewManager creates a new swarm manager for a rig.
func NewManager(r *rig.Rig) *Manager {
	return &Manager{
		rig:     r,
		swarms:  make(map[string]*Swarm),
		workDir: r.Path,
	}
}

// Create creates a new swarm from an epic.
func (m *Manager) Create(epicID string, workers []string, targetBranch string) (*Swarm, error) {
	if _, exists := m.swarms[epicID]; exists {
		return nil, ErrSwarmExists
	}

	// Get current git commit as base (optional - may not have git)
	baseCommit, _ := m.getGitHead()
	if baseCommit == "" {
		baseCommit = "unknown"
	}

	now := time.Now()
	swarm := &Swarm{
		ID:           epicID,
		RigName:      m.rig.Name,
		EpicID:       epicID,
		BaseCommit:   baseCommit,
		Integration:  fmt.Sprintf("swarm/%s", epicID),
		TargetBranch: targetBranch,
		State:        SwarmCreated,
		CreatedAt:    now,
		UpdatedAt:    now,
		Workers:      workers,
		Tasks:        []SwarmTask{},
	}

	// Load tasks from beads
	tasks, err := m.loadTasksFromBeads(epicID)
	if err != nil {
		// Non-fatal - swarm can start without tasks loaded
	} else {
		swarm.Tasks = tasks
	}

	m.swarms[epicID] = swarm
	return swarm, nil
}

// Start activates a swarm, transitioning from Created to Active.
func (m *Manager) Start(swarmID string) error {
	swarm, ok := m.swarms[swarmID]
	if !ok {
		return ErrSwarmNotFound
	}

	if swarm.State != SwarmCreated {
		return fmt.Errorf("%w: cannot start from state %s", ErrInvalidState, swarm.State)
	}

	swarm.State = SwarmActive
	swarm.UpdatedAt = time.Now()
	return nil
}

// UpdateState transitions the swarm to a new state.
func (m *Manager) UpdateState(swarmID string, state SwarmState) error {
	swarm, ok := m.swarms[swarmID]
	if !ok {
		return ErrSwarmNotFound
	}

	// Validate state transition
	if !isValidTransition(swarm.State, state) {
		return fmt.Errorf("%w: cannot transition from %s to %s",
			ErrInvalidState, swarm.State, state)
	}

	swarm.State = state
	swarm.UpdatedAt = time.Now()
	return nil
}

// Cancel cancels a swarm with a reason.
func (m *Manager) Cancel(swarmID string, reason string) error {
	swarm, ok := m.swarms[swarmID]
	if !ok {
		return ErrSwarmNotFound
	}

	if swarm.State.IsTerminal() {
		return fmt.Errorf("%w: swarm already in terminal state %s",
			ErrInvalidState, swarm.State)
	}

	swarm.State = SwarmCancelled
	swarm.Error = reason
	swarm.UpdatedAt = time.Now()
	return nil
}

// GetSwarm returns a swarm by ID.
func (m *Manager) GetSwarm(id string) (*Swarm, error) {
	swarm, ok := m.swarms[id]
	if !ok {
		return nil, ErrSwarmNotFound
	}
	return swarm, nil
}

// GetReadyTasks returns tasks ready to be assigned.
func (m *Manager) GetReadyTasks(swarmID string) ([]SwarmTask, error) {
	swarm, ok := m.swarms[swarmID]
	if !ok {
		return nil, ErrSwarmNotFound
	}

	var ready []SwarmTask
	for _, task := range swarm.Tasks {
		if task.State == TaskPending {
			ready = append(ready, task)
		}
	}

	if len(ready) == 0 {
		return nil, ErrNoReadyTasks
	}
	return ready, nil
}

// GetActiveTasks returns tasks currently in progress.
func (m *Manager) GetActiveTasks(swarmID string) ([]SwarmTask, error) {
	swarm, ok := m.swarms[swarmID]
	if !ok {
		return nil, ErrSwarmNotFound
	}

	var active []SwarmTask
	for _, task := range swarm.Tasks {
		if task.State == TaskInProgress || task.State == TaskAssigned {
			active = append(active, task)
		}
	}
	return active, nil
}

// IsComplete checks if all tasks are in terminal states.
func (m *Manager) IsComplete(swarmID string) (bool, error) {
	swarm, ok := m.swarms[swarmID]
	if !ok {
		return false, ErrSwarmNotFound
	}

	if len(swarm.Tasks) == 0 {
		return false, nil
	}

	for _, task := range swarm.Tasks {
		if !task.State.IsComplete() {
			return false, nil
		}
	}
	return true, nil
}

// AssignTask assigns a task to a worker.
func (m *Manager) AssignTask(swarmID, taskID, worker string) error {
	swarm, ok := m.swarms[swarmID]
	if !ok {
		return ErrSwarmNotFound
	}

	for i, task := range swarm.Tasks {
		if task.IssueID == taskID {
			swarm.Tasks[i].Assignee = worker
			swarm.Tasks[i].State = TaskAssigned
			swarm.Tasks[i].Branch = fmt.Sprintf("polecat/%s/%s", worker, taskID)
			swarm.UpdatedAt = time.Now()
			return nil
		}
	}
	return fmt.Errorf("task %s not found in swarm", taskID)
}

// UpdateTaskState updates a task's state.
func (m *Manager) UpdateTaskState(swarmID, taskID string, state TaskState) error {
	swarm, ok := m.swarms[swarmID]
	if !ok {
		return ErrSwarmNotFound
	}

	for i, task := range swarm.Tasks {
		if task.IssueID == taskID {
			swarm.Tasks[i].State = state
			if state == TaskMerged {
				now := time.Now()
				swarm.Tasks[i].MergedAt = &now
			}
			swarm.UpdatedAt = time.Now()
			return nil
		}
	}
	return fmt.Errorf("task %s not found in swarm", taskID)
}

// ListSwarms returns all swarms in the manager.
func (m *Manager) ListSwarms() []*Swarm {
	swarms := make([]*Swarm, 0, len(m.swarms))
	for _, s := range m.swarms {
		swarms = append(swarms, s)
	}
	return swarms
}

// ListActiveSwarms returns non-terminal swarms.
func (m *Manager) ListActiveSwarms() []*Swarm {
	var active []*Swarm
	for _, s := range m.swarms {
		if s.State.IsActive() {
			active = append(active, s)
		}
	}
	return active
}

// isValidTransition checks if a state transition is allowed.
func isValidTransition(from, to SwarmState) bool {
	transitions := map[SwarmState][]SwarmState{
		SwarmCreated:   {SwarmActive, SwarmCancelled},
		SwarmActive:    {SwarmMerging, SwarmFailed, SwarmCancelled},
		SwarmMerging:   {SwarmLanded, SwarmFailed, SwarmCancelled},
		SwarmLanded:    {}, // Terminal
		SwarmFailed:    {}, // Terminal
		SwarmCancelled: {}, // Terminal
	}

	allowed, ok := transitions[from]
	if !ok {
		return false
	}

	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

// loadTasksFromBeads loads child issues from beads CLI.
func (m *Manager) loadTasksFromBeads(epicID string) ([]SwarmTask, error) {
	// Run: bd show <epicID> --json to get epic with children
	cmd := exec.Command("bd", "show", epicID, "--json")
	cmd.Dir = m.workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("bd show: %s", strings.TrimSpace(stderr.String()))
	}

	// Parse JSON output - bd show returns an array
	var issues []struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		Status       string `json:"status"`
		Dependencies []struct {
			ID             string `json:"id"`
			Title          string `json:"title"`
			Status         string `json:"status"`
			DependencyType string `json:"dependency_type"`
		} `json:"dependencies"`
	}

	if err := json.Unmarshal(stdout.Bytes(), &issues); err != nil {
		return nil, fmt.Errorf("parsing bd output: %w", err)
	}

	if len(issues) == 0 {
		return nil, fmt.Errorf("epic not found: %s", epicID)
	}

	// Extract dependencies as tasks (issues that depend on/are blocked by this epic)
	// Accept both "parent-child" and "blocks" relationships
	var tasks []SwarmTask
	for _, dep := range issues[0].Dependencies {
		if dep.DependencyType != "parent-child" && dep.DependencyType != "blocks" {
			continue
		}

		state := TaskPending
		switch dep.Status {
		case "in_progress":
			state = TaskInProgress
		case "closed":
			state = TaskMerged
		}

		tasks = append(tasks, SwarmTask{
			IssueID: dep.ID,
			Title:   dep.Title,
			State:   state,
		})
	}

	return tasks, nil
}

// getGitHead returns the current HEAD commit.
func (m *Manager) getGitHead() (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = m.workDir

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return "", err
	}

	return strings.TrimSpace(stdout.String()), nil
}
