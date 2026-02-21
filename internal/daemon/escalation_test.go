package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEscalator_DedupKey(t *testing.T) {
	tmpDir := t.TempDir()
	e := NewEscalator(tmpDir, "gt", func(_ string, _ ...interface{}) {})

	tests := []struct {
		ctx      EscalationContext
		expected string
	}{
		{
			EscalationContext{Kind: KindDirtyPolecatState, Rig: "gastown", Polecat: "slit"},
			"gastown/slit",
		},
		{
			EscalationContext{Kind: KindCrashLoop, Rig: "gastown", Polecat: "toast"},
			"gastown/toast",
		},
		{
			EscalationContext{Kind: KindMergeConflict, Rig: "gastown", Branch: "polecat/slit/gt-abc"},
			"gastown/polecat/slit/gt-abc",
		},
		{
			EscalationContext{Kind: KindMassDeath, Rig: "gastown"},
			"gastown",
		},
		{
			EscalationContext{Kind: KindHealthFailures, Rig: "gastown", Polecat: "nux"},
			"gastown/nux",
		},
		{
			EscalationContext{Kind: KindHelpRequest, Rig: "gastown", HelpAgentID: "gastown/butter"},
			"gastown",
		},
	}

	for _, tt := range tests {
		got := e.buildDedupKey(tt.ctx)
		if got != tt.expected {
			t.Errorf("buildDedupKey(%v) = %q, want %q", tt.ctx.Kind, got, tt.expected)
		}
	}
}

func TestEscalator_BuildSubject(t *testing.T) {
	tmpDir := t.TempDir()
	e := NewEscalator(tmpDir, "gt", func(_ string, _ ...interface{}) {})

	tests := []struct {
		ctx     EscalationContext
		wantPfx string
	}{
		{
			EscalationContext{Kind: KindDirtyPolecatState, Rig: "gastown", Polecat: "slit", CleanupStatus: "has_unpushed"},
			"dirty state for polecat gastown/slit",
		},
		{
			EscalationContext{Kind: KindCrashLoop, Rig: "gastown", Polecat: "slit", Count: 6},
			"crash loop for polecat gastown/slit (6 restarts)",
		},
		{
			EscalationContext{Kind: KindMassDeath, Count: 3, Window: "30s"},
			"mass death: 3 sessions died in 30s",
		},
		{
			EscalationContext{Kind: KindHealthFailures, Rig: "gastown", Polecat: "slit", FailureCount: 5},
			"5 consecutive health check failures for gastown/slit",
		},
	}

	for _, tt := range tests {
		got := e.buildSubject(tt.ctx)
		if len(got) < len(tt.wantPfx) || got[:len(tt.wantPfx)] != tt.wantPfx {
			t.Errorf("buildSubject(%v) = %q, want prefix %q", tt.ctx.Kind, got, tt.wantPfx)
		}
	}
}

func TestEscalator_DedupPreventsRepeatSend(t *testing.T) {
	tmpDir := t.TempDir()

	var sentCount int
	// We can't intercept exec.Command, so we test via NotificationManager directly
	stateDir := filepath.Join(tmpDir, "daemon", "escalations")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	notifs := NewNotificationManager(stateDir, escalationDedup)

	ctx := EscalationContext{Kind: KindMassDeath, Rig: "gastown", Count: 3}

	// Simulate the dedup check logic
	sendIfReady := func() bool {
		key := "gastown"
		slot := string(ctx.Kind)
		ready, _ := notifs.SendIfReady(key, slot, "test subject")
		if ready {
			sentCount++
		}
		return ready
	}

	// First call should be ready
	if !sendIfReady() {
		t.Error("expected first send to be ready")
	}

	// Second call within 30 min should be suppressed
	if sendIfReady() {
		t.Error("expected second send to be suppressed by dedup")
	}

	if sentCount != 1 {
		t.Errorf("expected 1 send, got %d", sentCount)
	}
}

func TestHealthFailureTracker(t *testing.T) {
	tracker := NewHealthFailureTracker()

	// Start from zero
	if c := tracker.Count("agent-1"); c != 0 {
		t.Errorf("expected 0, got %d", c)
	}

	// Record some failures
	tracker.Record("agent-1")
	tracker.Record("agent-1")
	c := tracker.Record("agent-1")
	if c != 3 {
		t.Errorf("expected 3, got %d", c)
	}

	// Shouldn't affect agent-2
	if tracker.Count("agent-2") != 0 {
		t.Error("agent-2 should have 0 failures")
	}

	// Reset clears it
	tracker.Reset("agent-1")
	if tracker.Count("agent-1") != 0 {
		t.Error("expected 0 after reset")
	}
}

func TestEscalator_BuildBody(t *testing.T) {
	tmpDir := t.TempDir()
	e := NewEscalator(tmpDir, "gt", func(_ string, _ ...interface{}) {})

	ctx := EscalationContext{
		Kind:          KindDirtyPolecatState,
		Priority:      "high",
		Rig:           "gastown",
		Polecat:       "slit",
		CleanupStatus: "has_unpushed",
		AgentState:    "nuked",
	}

	body := e.buildBody(ctx)
	if body == "" {
		t.Error("expected non-empty body")
	}

	// Body should be valid JSON
	if body[0] != '{' {
		t.Errorf("expected JSON body, got: %s", body[:min(50, len(body))])
	}

	// Should contain escalated_at
	if !containsString(body, "escalated_at") {
		t.Error("body missing escalated_at field")
	}
}

// containsString checks if s contains substr.
func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && findSubstr(s, substr))
}

func findSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Ensure escalationDedup is a reasonable duration.
func TestEscalationDedup_Duration(t *testing.T) {
	if escalationDedup != 30*time.Minute {
		t.Errorf("escalationDedup = %v, want 30m", escalationDedup)
	}
}
