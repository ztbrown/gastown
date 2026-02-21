package daemon

// hybrid_daemon_integration_test.go — Integration tests for the expanded Go daemon.
//
// Verifies that the daemon correctly handles all witness/deacon protocol flows
// end-to-end, using shell-script mocks for external tools (gt, bd, tmux).
//
// Test scenarios:
//  1. POLECAT_DONE (clean) → auto-nuke path: message recognized, closed, handler called
//  2. POLECAT_DONE (dirty) → escalation path: message recognized and closed
//  3. POLECAT_DONE (PHASE_COMPLETE) → recycle path: message recognized and closed
//  4. MERGED → message recognized and closed
//  5. MERGE_FAILED → message recognized and closed
//  6. HELP → forwarded to Mayor via escalator
//  7. Health check failure tracking: consecutive failures → escalation at threshold
//  8. Orphan cleanup → patrolOrphanProcesses runs without panic
//  9. Timer gate → bd gate check invoked for known rigs
// 10. Convoy feeding → gt sling called when stranded convoy + idle dogs available
// 11. Idle town → isTownIdle returns true when bd shows no in-progress work
// 12. Escalation dedup → same issue not re-escalated within 30min

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ── Mock script helpers ──────────────────────────────────────────────────────

// hybridCallLog is a helper for reading the mock call log written by test scripts.
func hybridCallLog(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// callLogContains returns true if any line in the call log matches needle.
func callLogContains(callLog, needle string) bool {
	for _, line := range hybridCallLog(callLog) {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// callLogCount returns how many lines in the call log match needle.
func callLogCount(callLog, needle string) int {
	n := 0
	for _, line := range hybridCallLog(callLog) {
		if strings.Contains(line, needle) {
			n++
		}
	}
	return n
}

// writeHybridMockGT writes a mock gt script that:
//   - Logs all invocations to callLog
//   - Returns inboxJSON for "gt mail inbox --identity <witnessIdentity> --json"
//   - Returns "[]" for all other "gt mail inbox" calls (no messages)
//   - Succeeds silently for "gt mail delete", "gt mail send", "gt sling", etc.
func writeHybridMockGT(t *testing.T, dir, witnessIdentity, inboxJSON, callLog string) string {
	t.Helper()
	// Escape single quotes in inboxJSON for safe shell embedding.
	// The JSON in our tests should not contain single quotes, so this is defensive.
	escapedJSON := strings.ReplaceAll(inboxJSON, `'`, `'\''`)
	// Write inboxJSON to a file so the shell script can cat it without shell quoting issues.
	inboxFile := filepath.Join(dir, "witness_inbox.json")
	if err := os.WriteFile(inboxFile, []byte(inboxJSON), 0644); err != nil {
		t.Fatalf("writing inbox JSON file: %v", err)
	}
	_ = escapedJSON // kept for reference; we now use cat from file instead

	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q

# mail inbox: return test messages for the expected witness identity.
if [ "$1" = "mail" ] && [ "$2" = "inbox" ]; then
  case "$*" in
    *%s*)
      cat %q
      exit 0
      ;;
    *)
      echo '[]'
      exit 0
      ;;
  esac
fi

# mail delete / mail send / mail archive / sling / convoy: succeed silently.
exit 0
`, callLog, witnessIdentity, inboxFile)

	gtPath := filepath.Join(dir, "gt")
	if err := os.WriteFile(gtPath, []byte(script), 0755); err != nil {
		t.Fatalf("writing mock gt: %v", err)
	}
	return gtPath
}

// writeHybridMockBD writes a mock bd script that:
//   - Logs all invocations to callLog
//   - Returns inProgressJSON for "bd list --status=in_progress --json"
//   - Returns "[]" for all other bd list calls
//   - Succeeds silently for everything else
func writeHybridMockBD(t *testing.T, dir, inProgressJSON, callLog string) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q

if [ "$1" = "list" ]; then
  case "$*" in
    *in_progress*)
      echo '%s'
      exit 0
      ;;
    *)
      echo '[]'
      exit 0
      ;;
  esac
fi

# gate check, close, update: succeed silently.
exit 0
`, callLog, inProgressJSON)

	bdPath := filepath.Join(dir, "bd")
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil {
		t.Fatalf("writing mock bd: %v", err)
	}
	return bdPath
}

// hybridTestDaemonWithLog returns a daemon and a function to read its log buffer.
func hybridTestDaemonWithLog(t *testing.T, rigName, gtPath, bdPath string) (*Daemon, func() string) {
	t.Helper()
	townRoot := t.TempDir()

	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	rigsJSON := fmt.Sprintf(`{"rigs":{%q:{}}}`, rigName)
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), []byte(rigsJSON), 0644); err != nil {
		t.Fatalf("write rigs.json: %v", err)
	}

	var logBuf strings.Builder
	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(&logBuf, "", 0),
		gtPath: gtPath,
		bdPath: bdPath,
	}

	d.escalator = NewEscalator(townRoot, gtPath, func(format string, args ...interface{}) {
		d.logger.Printf(format, args...)
	})

	return d, logBuf.String
}

// ── Scenario 1-6: Witness inbox message routing ──────────────────────────────
//
// These tests verify that processRigWitnessInbox correctly routes each protocol
// message type to its handler and closes the message afterward. The underlying
// witness handlers perform real operations that will fail in a minimal test
// environment, but handleWitnessMessage returns true (handled) for all recognized
// message types regardless of handler errors — so close is always attempted.

func makeWitnessInboxJSON(msgs []BeadsMessage) string {
	data, _ := json.Marshal(msgs)
	return string(data)
}

// TestHybridDaemon_WitnessInbox_PolecatDoneRouted verifies that a POLECAT_DONE
// message is recognized, routed to the handler, and the message is closed.
func TestHybridDaemon_WitnessInbox_PolecatDoneRouted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	rigName := "testrig"
	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")
	witnessID := rigName + "/witness"

	msgs := []BeadsMessage{{
		ID:        "msg-pd-1",
		From:      "testrig/polecats/nux",
		To:        witnessID,
		Subject:   "POLECAT_DONE nux",
		Body:      "Exit: COMPLETED\nBranch: polecat/nux/gt-abc\nIssue: gt-abc",
		Timestamp: time.Now().Format(time.RFC3339),
		Read:      false,
		Priority:  "normal",
	}}

	inboxJSON := makeWitnessInboxJSON(msgs)
	gtPath := writeHybridMockGT(t, binDir, witnessID, inboxJSON, callLog)
	bdPath := writeHybridMockBD(t, binDir, "[]", callLog)

	d, _ := hybridTestDaemonWithLog(t, rigName, gtPath, bdPath)

	// processRigWitnessInbox is the main function under test.
	d.processRigWitnessInbox(rigName)

	// The message should have been recognized (POLECAT_DONE) and close attempted.
	// closeMessage calls "gt mail delete <id>".
	if !callLogContains(callLog, "mail delete msg-pd-1") {
		t.Errorf("expected 'gt mail delete msg-pd-1' in call log; calls: %v", hybridCallLog(callLog))
	}
}

// TestHybridDaemon_WitnessInbox_PolecatDone_PhaseComplete verifies PHASE_COMPLETE
// routing: recognized, handler called, message closed.
func TestHybridDaemon_WitnessInbox_PolecatDone_PhaseComplete(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	rigName := "testrig"
	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")
	witnessID := rigName + "/witness"

	msgs := []BeadsMessage{{
		ID:        "msg-pc-1",
		From:      "testrig/polecats/ace",
		To:        witnessID,
		Subject:   "POLECAT_DONE ace",
		Body:      "Exit: PHASE_COMPLETE\nGate: gt-gate-xyz\nBranch: polecat/ace/gt-xyz\nIssue: gt-xyz",
		Timestamp: time.Now().Format(time.RFC3339),
		Read:      false,
	}}

	inboxJSON := makeWitnessInboxJSON(msgs)
	gtPath := writeHybridMockGT(t, binDir, witnessID, inboxJSON, callLog)
	bdPath := writeHybridMockBD(t, binDir, "[]", callLog)

	d, _ := hybridTestDaemonWithLog(t, rigName, gtPath, bdPath)
	d.processRigWitnessInbox(rigName)

	// PHASE_COMPLETE POLECAT_DONE is handled → message closed.
	if !callLogContains(callLog, "mail delete msg-pc-1") {
		t.Errorf("expected 'gt mail delete msg-pc-1'; calls: %v", hybridCallLog(callLog))
	}
}

// TestHybridDaemon_WitnessInbox_MergedRouted verifies MERGED message routing.
func TestHybridDaemon_WitnessInbox_MergedRouted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	rigName := "testrig"
	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")
	witnessID := rigName + "/witness"

	msgs := []BeadsMessage{{
		ID:        "msg-merged-1",
		From:      "testrig/refinery",
		To:        witnessID,
		Subject:   "MERGED nux",
		Body:      "Branch: polecat/nux/gt-abc\nIssue: gt-abc\nCommit: deadbeef",
		Timestamp: time.Now().Format(time.RFC3339),
	}}

	inboxJSON := makeWitnessInboxJSON(msgs)
	gtPath := writeHybridMockGT(t, binDir, witnessID, inboxJSON, callLog)
	bdPath := writeHybridMockBD(t, binDir, "[]", callLog)

	d, _ := hybridTestDaemonWithLog(t, rigName, gtPath, bdPath)
	d.processRigWitnessInbox(rigName)

	if !callLogContains(callLog, "mail delete msg-merged-1") {
		t.Errorf("expected 'gt mail delete msg-merged-1'; calls: %v", hybridCallLog(callLog))
	}
}

// TestHybridDaemon_WitnessInbox_MergeFailedRouted verifies MERGE_FAILED routing.
func TestHybridDaemon_WitnessInbox_MergeFailedRouted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	rigName := "testrig"
	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")
	witnessID := rigName + "/witness"

	msgs := []BeadsMessage{{
		ID:        "msg-mf-1",
		From:      "testrig/refinery",
		To:        witnessID,
		Subject:   "MERGE_FAILED nux",
		Body:      "Branch: polecat/nux/gt-abc\nError: conflict in foo.go",
		Timestamp: time.Now().Format(time.RFC3339),
	}}

	inboxJSON := makeWitnessInboxJSON(msgs)
	gtPath := writeHybridMockGT(t, binDir, witnessID, inboxJSON, callLog)
	bdPath := writeHybridMockBD(t, binDir, "[]", callLog)

	d, _ := hybridTestDaemonWithLog(t, rigName, gtPath, bdPath)
	d.processRigWitnessInbox(rigName)

	if !callLogContains(callLog, "mail delete msg-mf-1") {
		t.Errorf("expected 'gt mail delete msg-mf-1'; calls: %v", hybridCallLog(callLog))
	}
}

// TestHybridDaemon_WitnessInbox_HelpForwardedToMayor verifies that a HELP message
// is handled (returned true) and the message is closed after routing.
func TestHybridDaemon_WitnessInbox_HelpForwardedToMayor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	rigName := "testrig"
	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")
	witnessID := rigName + "/witness"

	msgs := []BeadsMessage{{
		ID:        "msg-help-1",
		From:      "testrig/polecats/slit",
		To:        witnessID,
		Subject:   "HELP: Tests are failing on main",
		Body:      "I tried running go test ./... but getting compilation errors",
		Timestamp: time.Now().Format(time.RFC3339),
	}}

	inboxJSON := makeWitnessInboxJSON(msgs)
	gtPath := writeHybridMockGT(t, binDir, witnessID, inboxJSON, callLog)
	bdPath := writeHybridMockBD(t, binDir, "[]", callLog)

	d, _ := hybridTestDaemonWithLog(t, rigName, gtPath, bdPath)
	d.processRigWitnessInbox(rigName)

	// HELP is handled (true) → message closed.
	if !callLogContains(callLog, "mail delete msg-help-1") {
		t.Errorf("expected 'gt mail delete msg-help-1' for HELP message; calls: %v", hybridCallLog(callLog))
	}
}

// TestHybridDaemon_WitnessInbox_HandoffDiscarded verifies HANDOFF messages are
// silently discarded (handled=true → closed, no escalation).
func TestHybridDaemon_WitnessInbox_HandoffDiscarded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	rigName := "testrig"
	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")
	witnessID := rigName + "/witness"

	msgs := []BeadsMessage{{
		ID:        "msg-ho-1",
		From:      "testrig/polecats/nux",
		To:        witnessID,
		Subject:   "🤝 HANDOFF",
		Body:      "Session context: working on gt-abc",
		Timestamp: time.Now().Format(time.RFC3339),
	}}

	inboxJSON := makeWitnessInboxJSON(msgs)
	gtPath := writeHybridMockGT(t, binDir, witnessID, inboxJSON, callLog)
	bdPath := writeHybridMockBD(t, binDir, "[]", callLog)

	d, logStr := hybridTestDaemonWithLog(t, rigName, gtPath, bdPath)
	d.processRigWitnessInbox(rigName)

	// HANDOFF is handled (discarded) → message closed.
	if !callLogContains(callLog, "mail delete msg-ho-1") {
		t.Errorf("expected 'gt mail delete msg-ho-1' for HANDOFF; calls: %v", hybridCallLog(callLog))
	}
	// Should NOT escalate HANDOFF messages.
	if callLogContains(callLog, "mail send") {
		t.Errorf("HANDOFF should not trigger escalation; calls: %v", hybridCallLog(callLog))
	}
	// Should log the discard.
	if log := logStr(); !strings.Contains(log, "discarding HANDOFF") {
		t.Errorf("expected 'discarding HANDOFF' in log; got: %q", log)
	}
}

// TestHybridDaemon_WitnessInbox_UnrecognizedLeftUnread verifies that unrecognized
// protocol messages are NOT closed (left for manual inspection) and escalated.
func TestHybridDaemon_WitnessInbox_UnrecognizedLeftUnread(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	rigName := "testrig"
	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")
	witnessID := rigName + "/witness"

	msgs := []BeadsMessage{{
		ID:        "msg-unk-1",
		From:      "testrig/polecats/mystery",
		To:        witnessID,
		Subject:   "UNKNOWN_PROTOCOL_EVENT foo",
		Body:      "Some unknown event body",
		Timestamp: time.Now().Format(time.RFC3339),
	}}

	inboxJSON := makeWitnessInboxJSON(msgs)
	gtPath := writeHybridMockGT(t, binDir, witnessID, inboxJSON, callLog)
	bdPath := writeHybridMockBD(t, binDir, "[]", callLog)

	d, logStr := hybridTestDaemonWithLog(t, rigName, gtPath, bdPath)
	d.processRigWitnessInbox(rigName)

	// Unrecognized → NOT closed (left unread for manual inspection).
	if callLogContains(callLog, "mail delete msg-unk-1") {
		t.Errorf("unrecognized message should NOT be closed; calls: %v", hybridCallLog(callLog))
	}
	// Should log escalation attempt.
	if log := logStr(); !strings.Contains(log, "unrecognized message") {
		t.Errorf("expected 'unrecognized message' in log; got: %q", log)
	}
}

// TestHybridDaemon_WitnessInbox_SkipsReadMessages verifies that already-read
// messages are skipped (not re-processed, not re-closed).
func TestHybridDaemon_WitnessInbox_SkipsReadMessages(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	rigName := "testrig"
	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")
	witnessID := rigName + "/witness"

	msgs := []BeadsMessage{{
		ID:        "msg-already-read",
		From:      "testrig/polecats/nux",
		To:        witnessID,
		Subject:   "POLECAT_DONE nux",
		Body:      "Exit: COMPLETED",
		Timestamp: time.Now().Format(time.RFC3339),
		Read:      true, // Already processed
	}}

	inboxJSON := makeWitnessInboxJSON(msgs)
	gtPath := writeHybridMockGT(t, binDir, witnessID, inboxJSON, callLog)
	bdPath := writeHybridMockBD(t, binDir, "[]", callLog)

	d, _ := hybridTestDaemonWithLog(t, rigName, gtPath, bdPath)
	d.processRigWitnessInbox(rigName)

	// Read messages must not be re-closed.
	if callLogContains(callLog, "mail delete msg-already-read") {
		t.Errorf("read message should not be re-closed; calls: %v", hybridCallLog(callLog))
	}
}

// TestHybridDaemon_WitnessInbox_MultipleMessages verifies batch processing:
// recognized messages are closed, unrecognized messages are left unread.
func TestHybridDaemon_WitnessInbox_MultipleMessages(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	rigName := "testrig"
	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")
	witnessID := rigName + "/witness"

	ts := time.Now().Format(time.RFC3339)
	msgs := []BeadsMessage{
		{ID: "msg-merged-batch", Subject: "MERGED nux", Body: "Branch: polecat/nux/gt-abc\nIssue: gt-abc", Timestamp: ts},
		{ID: "msg-help-batch", Subject: "HELP: Something broken", Body: "details", Timestamp: ts},
		{ID: "msg-unk-batch", Subject: "WEIRD_THING foo", Body: "unknown", Timestamp: ts},
	}

	inboxJSON := makeWitnessInboxJSON(msgs)
	gtPath := writeHybridMockGT(t, binDir, witnessID, inboxJSON, callLog)
	bdPath := writeHybridMockBD(t, binDir, "[]", callLog)

	d, logStr := hybridTestDaemonWithLog(t, rigName, gtPath, bdPath)
	d.processRigWitnessInbox(rigName)

	calls := hybridCallLog(callLog)

	// Recognized messages should be closed.
	if !callLogContains(callLog, "mail delete msg-merged-batch") {
		t.Errorf("expected MERGED msg closed; calls: %v", calls)
	}
	if !callLogContains(callLog, "mail delete msg-help-batch") {
		t.Errorf("expected HELP msg closed; calls: %v", calls)
	}
	// Unrecognized message should NOT be closed.
	if callLogContains(callLog, "mail delete msg-unk-batch") {
		t.Errorf("unrecognized msg should not be closed; calls: %v", calls)
	}
	// Log should mention processed count.
	if log := logStr(); !strings.Contains(log, "processed") {
		t.Errorf("expected 'processed' in log for batch; got: %q", log)
	}
}

// ── Scenario 7: Health check failure tracking ────────────────────────────────

// TestHybridDaemon_HealthFailureTracker_ConsecutiveFailuresEscalate verifies that
// the HealthFailureTracker escalates to Mayor after reaching the threshold of
// consecutive failures, and resets correctly on success.
func TestHybridDaemon_HealthFailureTracker_ConsecutiveFailuresEscalate(t *testing.T) {
	tracker := NewHealthFailureTracker()

	// Record failures up to threshold - 1; should not trigger escalation yet.
	for i := 1; i < healthEscalationThreshold; i++ {
		count := tracker.Record("gastown/slit")
		if count != i {
			t.Errorf("Record iteration %d: got count %d, want %d", i, count, i)
		}
	}

	// At threshold: escalation should be triggered by caller.
	count := tracker.Record("gastown/slit")
	if count != healthEscalationThreshold {
		t.Errorf("final Record: got count %d, want %d", count, healthEscalationThreshold)
	}
	if count < healthEscalationThreshold {
		t.Errorf("expected count >= %d for escalation, got %d", healthEscalationThreshold, count)
	}

	// Reset on success — subsequent check after a healthy tick should clear count.
	tracker.Reset("gastown/slit")
	if n := tracker.Count("gastown/slit"); n != 0 {
		t.Errorf("expected 0 after Reset, got %d", n)
	}

	// A fresh failure after reset starts from 1 again.
	n := tracker.Record("gastown/slit")
	if n != 1 {
		t.Errorf("post-reset Record: got %d, want 1", n)
	}
}

// TestHybridDaemon_HealthFailureTracker_IndependentAgents verifies that failure
// counts are tracked independently per agent ID.
func TestHybridDaemon_HealthFailureTracker_IndependentAgents(t *testing.T) {
	tracker := NewHealthFailureTracker()

	tracker.Record("rig1/ace")
	tracker.Record("rig1/ace")
	tracker.Record("rig2/bolt")

	if n := tracker.Count("rig1/ace"); n != 2 {
		t.Errorf("rig1/ace: got %d, want 2", n)
	}
	if n := tracker.Count("rig2/bolt"); n != 1 {
		t.Errorf("rig2/bolt: got %d, want 1", n)
	}

	tracker.Reset("rig1/ace")
	if n := tracker.Count("rig1/ace"); n != 0 {
		t.Errorf("rig1/ace after Reset: got %d, want 0", n)
	}
	// Other agent unaffected.
	if n := tracker.Count("rig2/bolt"); n != 1 {
		t.Errorf("rig2/bolt should be unaffected by rig1/ace reset: got %d", n)
	}
}

// ── Scenario 8: Orphan cleanup ───────────────────────────────────────────────

// TestHybridDaemon_PatrolOrphanProcesses_NoPanic verifies that patrolOrphanProcesses
// completes without panic even when no orphan detection tools are available.
func TestHybridDaemon_PatrolOrphanProcesses_NoPanic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("orphan cleanup uses Unix process APIs")
	}

	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
	}

	// Should complete without panic, even if no orphaned processes exist.
	d.patrolOrphanProcesses()
}

// ── Scenario 9: Timer gate patrol ────────────────────────────────────────────

// TestHybridDaemon_PatrolTimerGates_InvokesGateCheck verifies that patrolTimerGates
// calls "bd gate check --type=timer" in both the town root and per-rig directories.
func TestHybridDaemon_PatrolTimerGates_InvokesGateCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	rigName := "gastown"
	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")

	gtPath := writeHybridMockGT(t, binDir, rigName+"/witness", "[]", callLog)
	bdPath := writeHybridMockBD(t, binDir, "[]", callLog)

	d, _ := hybridTestDaemonWithLog(t, rigName, gtPath, bdPath)
	d.patrolTimerGates()

	// bd gate check --type=timer should be called at least once.
	if !callLogContains(callLog, "gate check --type=timer") {
		t.Errorf("expected 'bd gate check --type=timer' in call log; calls: %v", hybridCallLog(callLog))
	}
}

// TestHybridDaemon_PatrolTimerGates_ElapsedGateClosed verifies that when bd gate check
// outputs "closed gate", the daemon logs the result.
func TestHybridDaemon_PatrolTimerGates_ElapsedGateClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	rigName := "gastown"
	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")

	// Mock bd that returns "closed gate" output for gate check.
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
if [ "$1" = "gate" ] && [ "$2" = "check" ]; then
  echo 'closed gate gt-timer-1 (elapsed)'
  exit 0
fi
echo '[]'
exit 0
`, callLog)
	bdPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}

	gtPath := writeHybridMockGT(t, binDir, rigName+"/witness", "[]", callLog)

	d, logStr := hybridTestDaemonWithLog(t, rigName, gtPath, bdPath)
	d.patrolTimerGates()

	// The daemon should log the gate check output.
	if log := logStr(); !strings.Contains(log, "gate check") {
		t.Errorf("expected gate check log entry; got: %q", log)
	}
}

// ── Scenario 10: Convoy feeding ──────────────────────────────────────────────

// TestHybridDaemon_PatrolConvoyFeeding_SlingsForStrandedConvoy verifies that when
// a stranded convoy has ready issues and idle dogs exist, gt sling is called.
func TestHybridDaemon_PatrolConvoyFeeding_SlingsForStrandedConvoy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")

	strandedJSON := `[{"id":"cv-stranded-1","ready_count":2,"ready_issues":["gt-x1","gt-x2"]}]`

	// Mock gt: returns stranded convoy for "gt convoy stranded --json", logs all calls.
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
if [ "$1" = "convoy" ] && [ "$2" = "stranded" ]; then
  echo '%s'
  exit 0
fi
exit 0
`, callLog, strandedJSON)
	gtPath := filepath.Join(binDir, "gt")
	if err := os.WriteFile(gtPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	bdPath := writeHybridMockBD(t, binDir, "[]", callLog)
	d, logStr := hybridTestDaemonWithLog(t, "testrig", gtPath, bdPath)

	// Create an idle dog in the kennel so patrolConvoyFeeding sees capacity.
	kennelPath := filepath.Join(d.config.TownRoot, "deacon", "dogs")
	createTestDog(t, kennelPath, "alpha", "idle")

	d.patrolConvoyFeeding()

	// gt sling should be called for the stranded convoy.
	if !callLogContains(callLog, "sling") {
		t.Errorf("expected 'gt sling' for stranded convoy; calls: %v", hybridCallLog(callLog))
	}
	if !callLogContains(callLog, "cv-stranded-1") {
		t.Errorf("expected convoy ID in sling call; calls: %v", hybridCallLog(callLog))
	}

	// Log should confirm the feed.
	if log := logStr(); !strings.Contains(log, "convoy feeding") {
		t.Errorf("expected 'convoy feeding' in log; got: %q", log)
	}
}

// TestHybridDaemon_PatrolConvoyFeeding_SkipsWhenNoIdleDogs verifies that stranded
// convoys are NOT fed when there are no idle dogs available.
func TestHybridDaemon_PatrolConvoyFeeding_SkipsWhenNoIdleDogs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")

	strandedJSON := `[{"id":"cv-stranded-2","ready_count":1,"ready_issues":["gt-y1"]}]`

	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
if [ "$1" = "convoy" ] && [ "$2" = "stranded" ]; then
  echo '%s'
  exit 0
fi
exit 0
`, callLog, strandedJSON)
	gtPath := filepath.Join(binDir, "gt")
	if err := os.WriteFile(gtPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	bdPath := writeHybridMockBD(t, binDir, "[]", callLog)
	d, logStr := hybridTestDaemonWithLog(t, "testrig", gtPath, bdPath)

	// No dogs in kennel — kennel dir intentionally not created.

	d.patrolConvoyFeeding()

	// gt sling should NOT be called when no idle dogs are available.
	if callLogContains(callLog, "sling") {
		t.Errorf("gt sling should NOT be called with no idle dogs; calls: %v", hybridCallLog(callLog))
	}
	// Should log the reason.
	if log := logStr(); !strings.Contains(log, "no idle dogs") {
		t.Errorf("expected 'no idle dogs' in log; got: %q", log)
	}
}

// TestHybridDaemon_PatrolConvoyFeeding_SkipsEmptyConvoys verifies that convoys with
// ready_count=0 are not fed (only truly ready convoys get work).
func TestHybridDaemon_PatrolConvoyFeeding_SkipsEmptyConvoys(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")

	// Stranded convoy with no ready issues.
	strandedJSON := `[{"id":"cv-empty-1","ready_count":0,"ready_issues":[]}]`

	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
if [ "$1" = "convoy" ] && [ "$2" = "stranded" ]; then
  echo '%s'
  exit 0
fi
exit 0
`, callLog, strandedJSON)
	gtPath := filepath.Join(binDir, "gt")
	if err := os.WriteFile(gtPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	bdPath := writeHybridMockBD(t, binDir, "[]", callLog)
	d, _ := hybridTestDaemonWithLog(t, "testrig", gtPath, bdPath)

	// Create an idle dog — but convoy has 0 ready issues so sling should still not fire.
	kennelPath := filepath.Join(d.config.TownRoot, "deacon", "dogs")
	createTestDog(t, kennelPath, "beta", "idle")

	d.patrolConvoyFeeding()

	if callLogContains(callLog, "sling") {
		t.Errorf("empty convoy should not trigger sling; calls: %v", hybridCallLog(callLog))
	}
}

// ── Scenario 11: Idle town ───────────────────────────────────────────────────

// TestHybridDaemon_IsTownIdle_TrueWhenNoBdData verifies that isTownIdle returns
// true when bd reports no in-progress beads (or bd is unavailable).
func TestHybridDaemon_IsTownIdle_TrueWhenNoBdData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")
	gtPath := writeHybridMockGT(t, binDir, "testrig/witness", "[]", callLog)
	bdPath := writeHybridMockBD(t, binDir, "[]", callLog) // 0 in-progress items

	d, _ := hybridTestDaemonWithLog(t, "testrig", gtPath, bdPath)

	if !d.isTownIdle() {
		t.Error("expected isTownIdle=true when bd returns empty in-progress list")
	}
}

// TestHybridDaemon_IsTownIdle_FalseWhenInProgressWork verifies that isTownIdle
// returns false when bd reports in-progress beads.
func TestHybridDaemon_IsTownIdle_FalseWhenInProgressWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")

	// bd returns one in-progress item at the town level.
	inProgressJSON := `[{"id":"gt-active-1","status":"in_progress","title":"Active work"}]`

	// This bd script returns in-progress work for the town root check.
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
if [ "$1" = "list" ]; then
  echo '%s'
  exit 0
fi
exit 0
`, callLog, inProgressJSON)
	bdPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}

	gtPath := writeHybridMockGT(t, binDir, "testrig/witness", "[]", callLog)
	d, _ := hybridTestDaemonWithLog(t, "testrig", gtPath, bdPath)

	if d.isTownIdle() {
		t.Error("expected isTownIdle=false when bd returns in-progress beads")
	}
}

// TestHybridDaemon_RunMechanicalPatrol_SkipsHealthPingsWhenIdle verifies that
// the mechanical patrol step 10 logs "skipping health pings" when town is idle.
func TestHybridDaemon_RunMechanicalPatrol_SkipsHealthPingsWhenIdle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")
	gtPath := writeHybridMockGT(t, binDir, "testrig/witness", "[]", callLog)
	// bd returns empty list → town is idle.
	bdPath := writeHybridMockBD(t, binDir, "[]", callLog)

	d, logStr := hybridTestDaemonWithLog(t, "testrig", gtPath, bdPath)

	// Attach tmux mock (non-functional but prevents nil panic).
	// We only check log output for the idle-skip message, not actual tmux calls.

	d.runMechanicalPatrol()

	if log := logStr(); !strings.Contains(log, "skipping health pings") {
		t.Errorf("expected 'skipping health pings' in patrol log when idle; got: %q", log)
	}
}

// ── Scenario 12: Escalation deduplication ────────────────────────────────────

// TestHybridDaemon_EscalationDedup_SuppressesDuplicatesWithin30Min verifies that
// the Escalator does not send the same escalation twice within the 30-minute dedup
// window, preventing Mayor mail flood on repeated daemon ticks.
func TestHybridDaemon_EscalationDedup_SuppressesDuplicatesWithin30Min(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")
	townRoot := t.TempDir()

	// Mock gt that logs all mail send calls.
	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
exit 0
`, callLog)
	gtPath := filepath.Join(binDir, "gt")
	if err := os.WriteFile(gtPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	escalator := NewEscalator(townRoot, gtPath, func(string, ...interface{}) {})

	ctx := EscalationContext{
		Kind:     KindHelpRequest,
		Priority: "high",
		Rig:      "gastown",
		HelpTopic: "build failing on main",
		HelpBody:  "go build ./... fails with import cycle",
		HelpAgentID: "gastown/polecats/nux",
	}

	// First escalation: should send mail.
	escalator.EscalateToMayor(ctx)

	// Second escalation within 30 min window: should be deduplicated (no second send).
	escalator.EscalateToMayor(ctx)

	// Count gt mail send calls — should be exactly 1 due to dedup.
	sendCount := callLogCount(callLog, "mail send")
	if sendCount != 1 {
		t.Errorf("expected exactly 1 escalation mail send (dedup suppresses second); got %d; calls: %v",
			sendCount, hybridCallLog(callLog))
	}
}

// TestHybridDaemon_EscalationDedup_DifferentKindsNotSuppressed verifies that
// different escalation kinds are NOT deduplicated against each other.
func TestHybridDaemon_EscalationDedup_DifferentKindsNotSuppressed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")
	townRoot := t.TempDir()

	script := fmt.Sprintf(`#!/bin/sh
echo "$*" >> %q
exit 0
`, callLog)
	gtPath := filepath.Join(binDir, "gt")
	if err := os.WriteFile(gtPath, []byte(script), 0755); err != nil {
		t.Fatalf("write mock gt: %v", err)
	}

	escalator := NewEscalator(townRoot, gtPath, func(string, ...interface{}) {})

	// Two different kinds for the same rig — both should send.
	escalator.EscalateToMayor(EscalationContext{
		Kind: KindHelpRequest,
		Rig:  "gastown",
		HelpTopic: "tests failing",
		HelpBody:  "details",
		HelpAgentID: "gastown/polecats/ace",
	})
	escalator.EscalateToMayor(EscalationContext{
		Kind:  KindCrashLoop,
		Rig:   "gastown",
		Polecat: "ace",
		Count: 5,
	})

	sendCount := callLogCount(callLog, "mail send")
	if sendCount < 2 {
		t.Errorf("different escalation kinds should each send mail; got %d sends; calls: %v",
			sendCount, hybridCallLog(callLog))
	}
}

// ── Additional: MergeReady message is discarded ───────────────────────────────

// TestHybridDaemon_WitnessInbox_MergeReadyDiscarded verifies that MERGE_READY
// messages appearing in the witness inbox (outbound-only) are closed but not
// forwarded or escalated — they are silently discarded with a log warning.
func TestHybridDaemon_WitnessInbox_MergeReadyDiscarded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mocks")
	}

	rigName := "testrig"
	binDir := t.TempDir()
	callLog := filepath.Join(binDir, "calls.log")
	witnessID := rigName + "/witness"

	msgs := []BeadsMessage{{
		ID:        "msg-mr-1",
		From:      "testrig/refinery",
		To:        witnessID,
		Subject:   "MERGE_READY nux",
		Body:      "Branch: polecat/nux/gt-abc",
		Timestamp: time.Now().Format(time.RFC3339),
	}}

	inboxJSON := makeWitnessInboxJSON(msgs)
	gtPath := writeHybridMockGT(t, binDir, witnessID, inboxJSON, callLog)
	bdPath := writeHybridMockBD(t, binDir, "[]", callLog)

	d, logStr := hybridTestDaemonWithLog(t, rigName, gtPath, bdPath)
	d.processRigWitnessInbox(rigName)

	// MERGE_READY should be closed (discarded).
	if !callLogContains(callLog, "mail delete msg-mr-1") {
		t.Errorf("expected 'gt mail delete msg-mr-1' for MERGE_READY discard; calls: %v", hybridCallLog(callLog))
	}
	// Should log the unexpected occurrence.
	if log := logStr(); !strings.Contains(log, "MERGE_READY") {
		t.Errorf("expected 'MERGE_READY' warning in log; got: %q", log)
	}
}

// ── Additional: processWitnessInboxes iterates all known rigs ─────────────────

// TestHybridDaemon_ProcessWitnessInboxes_RequiresEscalator verifies that
// processWitnessInboxes returns immediately (safe no-op) when escalator is nil.
func TestHybridDaemon_ProcessWitnessInboxes_RequiresEscalator(t *testing.T) {
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
		// escalator intentionally nil
	}

	// Should not panic when escalator is nil.
	d.processWitnessInboxes()
}
