package daemon

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ── countIdleDogs tests ──────────────────────────────────────────────────────

func TestCountIdleDogs_EmptyKennel(t *testing.T) {
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}}
	kennelPath := filepath.Join(d.config.TownRoot, "deacon", "dogs")

	count, err := d.countIdleDogs(kennelPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("empty kennel: got %d idle dogs, want 0", count)
	}
}

func TestCountIdleDogs_MixedStates(t *testing.T) {
	tmpDir := t.TempDir()
	d := &Daemon{config: &Config{TownRoot: tmpDir}}
	kennelPath := filepath.Join(tmpDir, "deacon", "dogs")

	// Create 2 idle dogs and 1 working dog
	createTestDog(t, kennelPath, "alpha", "idle")
	createTestDog(t, kennelPath, "beta", "idle")
	createTestDog(t, kennelPath, "gamma", "working")

	count, err := d.countIdleDogs(kennelPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Errorf("got %d idle dogs, want 2", count)
	}
}

func TestCountIdleDogs_AllWorking(t *testing.T) {
	tmpDir := t.TempDir()
	d := &Daemon{config: &Config{TownRoot: tmpDir}}
	kennelPath := filepath.Join(tmpDir, "deacon", "dogs")

	createTestDog(t, kennelPath, "alpha", "working")
	createTestDog(t, kennelPath, "beta", "working")

	count, err := d.countIdleDogs(kennelPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("got %d idle dogs, want 0", count)
	}
}

func TestCountIdleDogs_SkipsDotDirs(t *testing.T) {
	tmpDir := t.TempDir()
	d := &Daemon{config: &Config{TownRoot: tmpDir}}
	kennelPath := filepath.Join(tmpDir, "deacon", "dogs")

	// Create a hidden directory — should be ignored
	hiddenDir := filepath.Join(kennelPath, ".git")
	if err := os.MkdirAll(hiddenDir, 0755); err != nil {
		t.Fatal(err)
	}

	createTestDog(t, kennelPath, "alpha", "idle")

	count, err := d.countIdleDogs(kennelPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("got %d idle dogs, want 1", count)
	}
}

// ── isTownIdle tests ─────────────────────────────────────────────────────────

func TestIsTownIdle_NoBdCommand(t *testing.T) {
	// When bd is not available or returns error, countInProgress returns 0
	// (fail-open: treat error as empty = non-idle is wrong, so we return 0)
	// This means isTownIdle returns true when bd is broken — acceptable.
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		bdPath: "/nonexistent/bd",
	}

	// Should not panic
	result := d.isTownIdle()
	// When bd fails, countInProgress returns 0, so town appears idle
	if !result {
		t.Error("expected isTownIdle to return true when bd unavailable (fail-open)")
	}
}

// ── patrolLogRotation tests ──────────────────────────────────────────────────

func TestPatrolLogRotation_NoFile(t *testing.T) {
	tmpDir := t.TempDir()
	d := newTestDaemon(t, tmpDir)
	d.config.LogFile = filepath.Join(tmpDir, "nonexistent.log")

	// Should not panic or error when log doesn't exist
	d.patrolLogRotation()
}

func TestPatrolLogRotation_SmallFile(t *testing.T) {
	tmpDir := t.TempDir()
	d := newTestDaemon(t, tmpDir)
	logPath := filepath.Join(tmpDir, "daemon.log")
	d.config.LogFile = logPath

	// Write a small file — should not be rotated
	if err := os.WriteFile(logPath, []byte("small log"), 0644); err != nil {
		t.Fatal(err)
	}

	d.patrolLogRotation()

	// File should still exist with original content
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("log file disappeared: %v", err)
	}
	if string(content) != "small log" {
		t.Errorf("log content changed unexpectedly: %q", string(content))
	}

	// Rotated file should NOT exist
	if _, err := os.Stat(logPath + ".1"); !os.IsNotExist(err) {
		t.Error("unexpected rotated log file created for small log")
	}
}

func TestPatrolLogRotation_LargeFile(t *testing.T) {
	tmpDir := t.TempDir()
	d := newTestDaemon(t, tmpDir)
	logPath := filepath.Join(tmpDir, "daemon.log")
	d.config.LogFile = logPath

	// Write data larger than patrolLogMaxSize
	large := make([]byte, patrolLogMaxSize+1)
	for i := range large {
		large[i] = 'x'
	}
	if err := os.WriteFile(logPath, large, 0644); err != nil {
		t.Fatal(err)
	}

	d.patrolLogRotation()

	// Rotated file should exist
	rotated := logPath + ".1"
	if _, err := os.Stat(rotated); err != nil {
		t.Errorf("rotated log %s not created: %v", rotated, err)
	}

	// Original file should be truncated
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("log file disappeared after rotation: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("log file not truncated after rotation: size=%d", info.Size())
	}
}

// ── patrolDogHealth tests ────────────────────────────────────────────────────

func TestPatrolDogHealth_NoKennel(t *testing.T) {
	d := newTestDaemon(t, t.TempDir())
	// Should not panic when kennel doesn't exist
	d.patrolDogHealth()
}

func TestPatrolDogHealth_HealthyDog(t *testing.T) {
	tmpDir := t.TempDir()
	d := newTestDaemon(t, tmpDir)
	kennelPath := filepath.Join(tmpDir, "deacon", "dogs")

	// Create a working dog that just started
	createTestDogWithTime(t, kennelPath, "alpha", "working", time.Now())

	// Should not produce any warnings for a recently started dog
	d.patrolDogHealth()
}

func TestPatrolDogHealth_StuckDog(t *testing.T) {
	tmpDir := t.TempDir()
	d := newTestDaemon(t, tmpDir)
	kennelPath := filepath.Join(tmpDir, "deacon", "dogs")

	// Create a dog working for too long
	stuckSince := time.Now().Add(-(patrolDogMaxWorkDuration + 1*time.Hour))
	createTestDogWithTime(t, kennelPath, "alpha", "working", stuckSince)

	// Should not panic — stuck dog is logged but not killed
	d.patrolDogHealth()
}

// ── helpers ──────────────────────────────────────────────────────────────────

func createTestDog(t *testing.T, kennelPath, name, state string) {
	t.Helper()
	createTestDogWithTime(t, kennelPath, name, state, time.Now())
}

func createTestDogWithTime(t *testing.T, kennelPath, name, state string, lastActive time.Time) {
	t.Helper()
	dogDir := filepath.Join(kennelPath, name)
	if err := os.MkdirAll(dogDir, 0755); err != nil {
		t.Fatalf("creating dog dir %s: %v", dogDir, err)
	}

	dogState := map[string]interface{}{
		"name":        name,
		"state":       state,
		"last_active": lastActive,
		"created_at":  time.Now(),
		"updated_at":  time.Now(),
	}
	data, err := json.Marshal(dogState)
	if err != nil {
		t.Fatalf("marshaling dog state: %v", err)
	}

	stateFile := filepath.Join(dogDir, dogStateFile)
	if err := os.WriteFile(stateFile, data, 0644); err != nil {
		t.Fatalf("writing dog state %s: %v", stateFile, err)
	}
}

func newTestDaemon(t *testing.T, tmpDir string) *Daemon {
	t.Helper()

	logFile := filepath.Join(tmpDir, "daemon.log")

	return &Daemon{
		config: &Config{
			TownRoot: tmpDir,
			LogFile:  logFile,
		},
		logger: log.New(io.Discard, "", 0),
		gtPath: "/nonexistent/gt", // tests don't invoke gt
		bdPath: "/nonexistent/bd", // tests don't invoke bd
	}
}
