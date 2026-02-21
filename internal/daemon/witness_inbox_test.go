package daemon

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/mail"
)

func TestWitnessBeadsToMail_BasicConversion(t *testing.T) {
	msg := &BeadsMessage{
		ID:        "test-id-123",
		From:      "rig1/polecat1",
		To:        "rig1/witness",
		Subject:   "POLECAT_DONE polecat1",
		Body:      "Exit: COMPLETED\nIssue: gt-abc",
		Timestamp: "2025-01-15T10:00:00Z",
		Read:      false,
		Priority:  "normal",
		Type:      "notification",
	}

	result := witnessBeadsToMail(msg)

	if result.ID != msg.ID {
		t.Errorf("ID: got %q, want %q", result.ID, msg.ID)
	}
	if result.From != msg.From {
		t.Errorf("From: got %q, want %q", result.From, msg.From)
	}
	if result.Subject != msg.Subject {
		t.Errorf("Subject: got %q, want %q", result.Subject, msg.Subject)
	}
	if result.Body != msg.Body {
		t.Errorf("Body: got %q, want %q", result.Body, msg.Body)
	}
	if result.Read != msg.Read {
		t.Errorf("Read: got %v, want %v", result.Read, msg.Read)
	}
	if result.Priority != mail.PriorityNormal {
		t.Errorf("Priority: got %v, want PriorityNormal", result.Priority)
	}
	if result.Type != mail.TypeNotification {
		t.Errorf("Type: got %v, want TypeNotification", result.Type)
	}

	wantTime, _ := time.Parse(time.RFC3339, "2025-01-15T10:00:00Z")
	if !result.Timestamp.Equal(wantTime) {
		t.Errorf("Timestamp: got %v, want %v", result.Timestamp, wantTime)
	}
}

func TestWitnessBeadsToMail_PriorityMapping(t *testing.T) {
	cases := []struct {
		input string
		want  mail.Priority
	}{
		{"urgent", mail.PriorityUrgent},
		{"high", mail.PriorityHigh},
		{"low", mail.PriorityLow},
		{"normal", mail.PriorityNormal},
		{"", mail.PriorityNormal},
		{"unknown", mail.PriorityNormal},
	}

	for _, c := range cases {
		msg := &BeadsMessage{Priority: c.input, Timestamp: time.Now().Format(time.RFC3339)}
		result := witnessBeadsToMail(msg)
		if result.Priority != c.want {
			t.Errorf("priority %q: got %v, want %v", c.input, result.Priority, c.want)
		}
	}
}

func TestWitnessBeadsToMail_TypeMapping(t *testing.T) {
	cases := []struct {
		input string
		want  mail.MessageType
	}{
		{"task", mail.TypeTask},
		{"scavenge", mail.TypeScavenge},
		{"reply", mail.TypeReply},
		{"notification", mail.TypeNotification},
		{"", mail.TypeNotification},
		{"unknown", mail.TypeNotification},
	}

	for _, c := range cases {
		msg := &BeadsMessage{Type: c.input, Timestamp: time.Now().Format(time.RFC3339)}
		result := witnessBeadsToMail(msg)
		if result.Type != c.want {
			t.Errorf("type %q: got %v, want %v", c.input, result.Type, c.want)
		}
	}
}

func TestWitnessBeadsToMail_BadTimestampFallback(t *testing.T) {
	before := time.Now()
	msg := &BeadsMessage{Timestamp: "not-a-valid-timestamp"}
	result := witnessBeadsToMail(msg)
	after := time.Now()

	// Should fall back to time.Now(), so must be between before and after
	if result.Timestamp.Before(before) || result.Timestamp.After(after) {
		t.Errorf("bad timestamp fallback: got %v, want between %v and %v",
			result.Timestamp, before, after)
	}
}

func TestHandleWitnessMessage_HandoffDiscarded(t *testing.T) {
	d := testDaemon()

	msg := &BeadsMessage{
		ID:      "handoff-msg-1",
		Subject: "🤝 HANDOFF",
		Body:    "Session context...",
	}

	handled := d.handleWitnessMessage("testrig", "/tmp/testrig", nil, msg)
	if !handled {
		t.Error("HANDOFF message should be handled (discarded) = true")
	}
}

func TestHandleWitnessMessage_MergeReadyDiscarded(t *testing.T) {
	d := testDaemon()

	msg := &BeadsMessage{
		ID:      "merge-ready-msg-1",
		Subject: "MERGE_READY polecat1",
		Body:    "Branch: polecat/polecat1/gt-abc\nIssue: gt-abc",
	}

	handled := d.handleWitnessMessage("testrig", "/tmp/testrig", nil, msg)
	if !handled {
		t.Error("MERGE_READY message should be handled (discarded) = true")
	}
}

func TestHandleWitnessMessage_UnrecognizedEscalated(t *testing.T) {
	d := testDaemon()

	// escalator needed for unrecognized messages — use NewEscalator to get a valid state
	tmpDir := t.TempDir()
	var escalated bool
	d.escalator = NewEscalator(tmpDir, "gt", func(format string, args ...interface{}) {
		escalated = true
	})

	msg := &BeadsMessage{
		ID:      "unknown-msg-1",
		Subject: "SOME_UNKNOWN_PROTOCOL_MESSAGE foo",
		Body:    "No idea what this is",
	}

	handled := d.handleWitnessMessage("testrig", "/tmp/testrig", nil, msg)
	if handled {
		t.Error("unrecognized message should return handled=false (left unread)")
	}
	_ = escalated // escalation side-effects tested separately via escalation_test.go
}

func TestLogWitnessResult_NilSafe(t *testing.T) {
	d := testDaemon()
	// Should not panic on nil result
	d.logWitnessResult("rig1", "POLECAT_DONE", "msg-1", nil)
}
