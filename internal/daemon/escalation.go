package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// EscalationKind identifies the type of escalation.
type EscalationKind string

const (
	// KindDirtyPolecatState: polecat has non-clean cleanup_status after POLECAT_DONE.
	KindDirtyPolecatState EscalationKind = "dirty_polecat_state"

	// KindHelpRequest: polecat sent a HELP: message that couldn't be resolved locally.
	KindHelpRequest EscalationKind = "help_request"

	// KindCrashLoop: agent is restarting repeatedly (crash loop detected).
	KindCrashLoop EscalationKind = "crash_loop"

	// KindMergeConflict: refinery reported a merge conflict needing a decision.
	KindMergeConflict EscalationKind = "merge_conflict"

	// KindHealthFailures: 5+ consecutive health check failures for an agent.
	KindHealthFailures EscalationKind = "health_failures"

	// KindMassDeath: 3+ sessions died within a 30-second window (systemic issue).
	KindMassDeath EscalationKind = "mass_death"
)

// escalationDedup is how long to suppress repeated escalations of the same issue.
const escalationDedup = 30 * time.Minute

// EscalationContext holds structured context for a Mayor escalation.
// Populate only the fields relevant to the escalation kind.
type EscalationContext struct {
	Kind     EscalationKind
	Priority string // "high" (default) or "urgent"

	// Agent context
	Rig     string // e.g., "gastown"
	Polecat string // polecat name, e.g., "slit"
	BeadID  string // related bead ID (task or agent bead)

	// State context
	AgentState    string // last known agent state
	HookBead      string // hooked work bead ID
	CleanupStatus string // for dirty polecat state
	Branch        string // for merge conflicts

	// Problem context
	ErrorDetails string   // error message or details
	Sessions     []string // for mass death events
	Count        int      // restart count or death count
	Window       string   // time window (e.g., "30s")
	FailureCount int      // consecutive failure count

	// For HELP forwarding: forward the original message verbatim
	HelpTopic   string
	HelpBody    string
	HelpAgentID string
}

// escalationBody is the structured JSON body sent to Mayor.
type escalationBody struct {
	Kind          string   `json:"kind"`
	Rig           string   `json:"rig,omitempty"`
	Polecat       string   `json:"polecat,omitempty"`
	BeadID        string   `json:"bead_id,omitempty"`
	AgentState    string   `json:"agent_state,omitempty"`
	HookBead      string   `json:"hook_bead,omitempty"`
	CleanupStatus string   `json:"cleanup_status,omitempty"`
	Branch        string   `json:"branch,omitempty"`
	ErrorDetails  string   `json:"error_details,omitempty"`
	Sessions      []string `json:"sessions,omitempty"`
	Count         int      `json:"count,omitempty"`
	Window        string   `json:"window,omitempty"`
	FailureCount  int      `json:"failure_count,omitempty"`
	HelpTopic     string   `json:"help_topic,omitempty"`
	HelpBody      string   `json:"help_body,omitempty"`
	HelpAgentID   string   `json:"help_agent_id,omitempty"`
	EscalatedAt   string   `json:"escalated_at"`
}

// Escalator sends structured escalation mail to Mayor with 30-min deduplication.
type Escalator struct {
	townRoot string
	gtPath   string
	notifs   *NotificationManager
	logger   func(string, ...interface{})
}

// NewEscalator creates an Escalator backed by a NotificationManager for dedup.
func NewEscalator(townRoot, gtPath string, logger func(string, ...interface{})) *Escalator {
	stateDir := filepath.Join(townRoot, "daemon", "escalations")
	return &Escalator{
		townRoot: townRoot,
		gtPath:   gtPath,
		notifs:   NewNotificationManager(stateDir, escalationDedup),
		logger:   logger,
	}
}

// EscalateToMayor sends a structured escalation mail to Mayor.
// Dedup: the same (kind, dedupKey) won't re-escalate within 30 minutes.
func (e *Escalator) EscalateToMayor(ctx EscalationContext) {
	dedupKey := e.buildDedupKey(ctx)

	// Subject is the human-readable one-liner
	subject := e.buildSubject(ctx)

	// Atomic check-and-record to prevent TOCTOU races
	ready, err := e.notifs.SendIfReady(dedupKey, string(ctx.Kind), subject)
	if err != nil {
		e.logger("Warning: escalation dedup check failed for %s: %v", ctx.Kind, err)
		// Fall through: allow send on dedup error
	}
	if !ready {
		e.logger("Escalation suppressed (30m dedup): %s for %s", ctx.Kind, dedupKey)
		return
	}

	priority := ctx.Priority
	if priority == "" {
		priority = "high"
	}

	body := e.buildBody(ctx)

	cmd := exec.Command(e.gtPath, "mail", "send", "mayor/", //nolint:gosec // G204: args constructed internally
		"-s", "ESCALATION: "+subject,
		"-m", body,
		"--priority", priority,
	)
	cmd.Dir = e.townRoot
	cmd.Env = os.Environ()

	if err := cmd.Run(); err != nil {
		e.logger("Warning: failed to escalate to Mayor (%s): %v", ctx.Kind, err)
	} else {
		e.logger("Escalated to Mayor: %s", subject)
	}
}

// buildDedupKey builds a stable key for deduplication.
// Same kind + agent/session combination → same key.
func (e *Escalator) buildDedupKey(ctx EscalationContext) string {
	switch ctx.Kind {
	case KindDirtyPolecatState, KindHelpRequest, KindCrashLoop, KindHealthFailures:
		if ctx.Polecat != "" {
			return fmt.Sprintf("%s/%s", ctx.Rig, ctx.Polecat)
		}
		return ctx.Rig
	case KindMergeConflict:
		if ctx.Branch != "" {
			return fmt.Sprintf("%s/%s", ctx.Rig, ctx.Branch)
		}
		return ctx.Rig
	case KindMassDeath:
		return ctx.Rig
	default:
		return string(ctx.Kind)
	}
}

// buildSubject builds a clear one-line subject for the escalation.
func (e *Escalator) buildSubject(ctx EscalationContext) string {
	switch ctx.Kind {
	case KindDirtyPolecatState:
		return fmt.Sprintf("dirty state for polecat %s/%s (cleanup_status=%s)",
			ctx.Rig, ctx.Polecat, ctx.CleanupStatus)
	case KindHelpRequest:
		topic := ctx.HelpTopic
		if topic == "" {
			topic = "unknown"
		}
		agent := ctx.HelpAgentID
		if agent == "" && ctx.Polecat != "" {
			agent = fmt.Sprintf("%s/%s", ctx.Rig, ctx.Polecat)
		}
		return fmt.Sprintf("HELP from %s: %s", agent, topic)
	case KindCrashLoop:
		if ctx.Polecat != "" {
			return fmt.Sprintf("crash loop for polecat %s/%s (%d restarts)", ctx.Rig, ctx.Polecat, ctx.Count)
		}
		return fmt.Sprintf("crash loop for agent %s (%d restarts)", ctx.Rig, ctx.Count)
	case KindMergeConflict:
		return fmt.Sprintf("merge conflict in %s (branch=%s)", ctx.Rig, ctx.Branch)
	case KindHealthFailures:
		if ctx.Polecat != "" {
			return fmt.Sprintf("%d consecutive health check failures for %s/%s", ctx.FailureCount, ctx.Rig, ctx.Polecat)
		}
		return fmt.Sprintf("%d consecutive health check failures for %s", ctx.FailureCount, ctx.Rig)
	case KindMassDeath:
		return fmt.Sprintf("mass death: %d sessions died in %s", ctx.Count, ctx.Window)
	default:
		return fmt.Sprintf("escalation kind=%s rig=%s", ctx.Kind, ctx.Rig)
	}
}

// buildBody builds the JSON body with full context.
func (e *Escalator) buildBody(ctx EscalationContext) string {
	body := escalationBody{
		Kind:          string(ctx.Kind),
		Rig:           ctx.Rig,
		Polecat:       ctx.Polecat,
		BeadID:        ctx.BeadID,
		AgentState:    ctx.AgentState,
		HookBead:      ctx.HookBead,
		CleanupStatus: ctx.CleanupStatus,
		Branch:        ctx.Branch,
		ErrorDetails:  ctx.ErrorDetails,
		Sessions:      ctx.Sessions,
		Count:         ctx.Count,
		Window:        ctx.Window,
		FailureCount:  ctx.FailureCount,
		HelpTopic:     ctx.HelpTopic,
		HelpBody:      ctx.HelpBody,
		HelpAgentID:   ctx.HelpAgentID,
		EscalatedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		// Fallback to plain text if JSON fails
		return fmt.Sprintf("kind: %s\nrig: %s\npolecat: %s\nerror: %v",
			ctx.Kind, ctx.Rig, ctx.Polecat, err)
	}
	return string(data)
}

// HealthFailureTracker tracks consecutive health check failures per agent.
// Not safe for concurrent use — only call from the heartbeat goroutine.
type HealthFailureTracker struct {
	failures map[string]int
}

// NewHealthFailureTracker creates a new tracker.
func NewHealthFailureTracker() *HealthFailureTracker {
	return &HealthFailureTracker{
		failures: make(map[string]int),
	}
}

// Record increments the failure count for an agent and returns the new count.
func (t *HealthFailureTracker) Record(agentID string) int {
	t.failures[agentID]++
	return t.failures[agentID]
}

// Reset clears the failure count for an agent (call on successful check).
func (t *HealthFailureTracker) Reset(agentID string) {
	delete(t.failures, agentID)
}

// Count returns the current failure count for an agent.
func (t *HealthFailureTracker) Count(agentID string) int {
	return t.failures[agentID]
}

// healthEscalationThreshold is the number of consecutive failures before escalating.
const healthEscalationThreshold = 5
