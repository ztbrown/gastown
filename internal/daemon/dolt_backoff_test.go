package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdvanceBackoff(t *testing.T) {
	m := &DoltServerManager{
		config: &DoltServerConfig{
			RestartDelay:    5 * time.Second,
			MaxRestartDelay: 5 * time.Minute,
		},
		logger: func(format string, v ...interface{}) {},
	}

	// First advance: 5s -> 10s
	m.advanceBackoff()
	if m.currentDelay != 10*time.Second {
		t.Errorf("expected 10s, got %v", m.currentDelay)
	}

	// Second advance: 10s -> 20s
	m.advanceBackoff()
	if m.currentDelay != 20*time.Second {
		t.Errorf("expected 20s, got %v", m.currentDelay)
	}

	// Third: 20s -> 40s
	m.advanceBackoff()
	if m.currentDelay != 40*time.Second {
		t.Errorf("expected 40s, got %v", m.currentDelay)
	}

	// Fourth: 40s -> 80s
	m.advanceBackoff()
	if m.currentDelay != 80*time.Second {
		t.Errorf("expected 80s, got %v", m.currentDelay)
	}

	// Fifth: 80s -> 160s
	m.advanceBackoff()
	if m.currentDelay != 160*time.Second {
		t.Errorf("expected 160s, got %v", m.currentDelay)
	}

	// Sixth: 160s -> 300s (capped at 5min)
	m.advanceBackoff()
	if m.currentDelay != 5*time.Minute {
		t.Errorf("expected 5m0s (cap), got %v", m.currentDelay)
	}

	// Stays capped
	m.advanceBackoff()
	if m.currentDelay != 5*time.Minute {
		t.Errorf("expected 5m0s (still capped), got %v", m.currentDelay)
	}
}

func TestGetBackoffDelay_InitialValue(t *testing.T) {
	m := &DoltServerManager{
		config: &DoltServerConfig{
			RestartDelay: 5 * time.Second,
		},
		logger: func(format string, v ...interface{}) {},
	}

	// Before any advances, should return base delay
	delay := m.getBackoffDelay()
	if delay != 5*time.Second {
		t.Errorf("expected initial delay 5s, got %v", delay)
	}
}

func TestPruneRestartTimes(t *testing.T) {
	now := time.Now()
	m := &DoltServerManager{
		config: &DoltServerConfig{
			RestartWindow: 10 * time.Minute,
		},
		logger: func(format string, v ...interface{}) {},
		restartTimes: []time.Time{
			now.Add(-15 * time.Minute), // Outside window
			now.Add(-11 * time.Minute), // Outside window
			now.Add(-5 * time.Minute),  // Inside window
			now.Add(-1 * time.Minute),  // Inside window
		},
	}

	m.pruneRestartTimes(now)

	if len(m.restartTimes) != 2 {
		t.Errorf("expected 2 times after pruning, got %d", len(m.restartTimes))
	}
}

func TestMaybeResetBackoff(t *testing.T) {
	m := &DoltServerManager{
		config: &DoltServerConfig{
			HealthyResetInterval: 5 * time.Minute,
		},
		logger:       func(format string, v ...interface{}) {},
		currentDelay: 40 * time.Second,
		restartTimes: []time.Time{time.Now()},
		escalated:    true,
	}

	// First call sets lastHealthyTime
	m.maybeResetBackoff()
	if m.currentDelay != 40*time.Second {
		t.Error("should not reset on first healthy check")
	}

	// Simulate time passing (set lastHealthyTime to 6 minutes ago)
	m.lastHealthyTime = time.Now().Add(-6 * time.Minute)
	m.maybeResetBackoff()

	if m.currentDelay != 0 {
		t.Errorf("expected delay reset to 0, got %v", m.currentDelay)
	}
	if m.restartTimes != nil {
		t.Error("expected restartTimes to be nil after reset")
	}
	if m.escalated {
		t.Error("expected escalated to be false after reset")
	}
}

func TestMaybeResetBackoff_NoResetIfNotLongEnough(t *testing.T) {
	m := &DoltServerManager{
		config: &DoltServerConfig{
			HealthyResetInterval: 5 * time.Minute,
		},
		logger:          func(format string, v ...interface{}) {},
		currentDelay:    40 * time.Second,
		lastHealthyTime: time.Now().Add(-2 * time.Minute), // Only 2 min healthy
		restartTimes:    []time.Time{time.Now()},
	}

	m.maybeResetBackoff()

	if m.currentDelay != 40*time.Second {
		t.Errorf("should not reset after only 2 minutes, got delay %v", m.currentDelay)
	}
}

func TestMaybeResetBackoff_AccumulatesAcrossHeartbeats(t *testing.T) {
	// Regression test: with the bug, lastHealthyTime was updated on every call,
	// so the delta never exceeded the heartbeat interval. With the fix,
	// lastHealthyTime is only updated on initial detection and after a successful
	// reset, allowing the delta to accumulate across multiple heartbeat calls.
	m := &DoltServerManager{
		config: &DoltServerConfig{
			HealthyResetInterval: 10 * time.Minute,
		},
		logger:       func(format string, v ...interface{}) {},
		currentDelay: 40 * time.Second,
		restartTimes: []time.Time{time.Now()},
		escalated:    true,
	}

	// First call: sets lastHealthyTime to now
	m.maybeResetBackoff()
	if m.currentDelay != 40*time.Second {
		t.Fatal("should not reset on first call")
	}

	// Simulate calling every 1 minute for 9 minutes (short heartbeats).
	// With the bug, each call reset lastHealthyTime so delta was always ~1min.
	// With the fix, lastHealthyTime stays at the initial value.
	for i := 1; i <= 9; i++ {
		m.maybeResetBackoff()
	}
	// After 9 calls at ~0 delta each (in test time), still should not reset
	// because no real time has passed. But importantly, lastHealthyTime should
	// NOT have been updated on these calls.
	if m.currentDelay != 40*time.Second {
		t.Fatal("should not have reset yet")
	}

	// Now set lastHealthyTime to 11 minutes ago (simulating accumulated healthy time)
	// This should trigger a reset because the initial healthy detection was >10min ago.
	m.lastHealthyTime = time.Now().Add(-11 * time.Minute)
	m.maybeResetBackoff()

	if m.currentDelay != 0 {
		t.Errorf("expected delay reset to 0 after 11 minutes healthy, got %v", m.currentDelay)
	}
	if m.escalated {
		t.Error("expected escalated to be false after reset")
	}
}

func TestDefaultConfig_BackoffFields(t *testing.T) {
	cfg := DefaultDoltServerConfig("/tmp/test")

	if cfg.MaxRestartDelay != 5*time.Minute {
		t.Errorf("expected MaxRestartDelay 5m, got %v", cfg.MaxRestartDelay)
	}
	if cfg.MaxRestartsInWindow != 5 {
		t.Errorf("expected MaxRestartsInWindow 5, got %d", cfg.MaxRestartsInWindow)
	}
	if cfg.RestartWindow != 10*time.Minute {
		t.Errorf("expected RestartWindow 10m, got %v", cfg.RestartWindow)
	}
	if cfg.HealthyResetInterval != 5*time.Minute {
		t.Errorf("expected HealthyResetInterval 5m, got %v", cfg.HealthyResetInterval)
	}
	if cfg.HealthCheckInterval != DefaultDoltHealthCheckInterval {
		t.Errorf("expected HealthCheckInterval %v, got %v", DefaultDoltHealthCheckInterval, cfg.HealthCheckInterval)
	}
}

func TestHealthCheckInterval_Default(t *testing.T) {
	m := &DoltServerManager{
		config: &DoltServerConfig{
			Enabled: true,
		},
		logger: func(format string, v ...interface{}) {},
	}

	// When HealthCheckInterval is not set (zero), should return default
	interval := m.HealthCheckInterval()
	if interval != DefaultDoltHealthCheckInterval {
		t.Errorf("expected default %v, got %v", DefaultDoltHealthCheckInterval, interval)
	}
}

func TestHealthCheckInterval_Configured(t *testing.T) {
	m := &DoltServerManager{
		config: &DoltServerConfig{
			Enabled:             true,
			HealthCheckInterval: 15 * time.Second,
		},
		logger: func(format string, v ...interface{}) {},
	}

	interval := m.HealthCheckInterval()
	if interval != 15*time.Second {
		t.Errorf("expected 15s, got %v", interval)
	}
}

func TestHealthCheckInterval_NilConfig(t *testing.T) {
	m := &DoltServerManager{
		config: nil,
		logger: func(format string, v ...interface{}) {},
	}

	interval := m.HealthCheckInterval()
	if interval != DefaultDoltHealthCheckInterval {
		t.Errorf("expected default %v with nil config, got %v", DefaultDoltHealthCheckInterval, interval)
	}
}

func TestRestartingFlag_PreventsConcurrentRestarts(t *testing.T) {
	// Verify the restarting flag prevents concurrent calls to EnsureRunning
	// from both entering restartWithBackoff.
	var callCount atomic.Int32
	m := &DoltServerManager{
		config: &DoltServerConfig{
			Enabled:             true,
			Port:                13306, // Non-standard port to avoid conflicts
			Host:                "127.0.0.1",
			RestartDelay:        50 * time.Millisecond,
			MaxRestartDelay:     100 * time.Millisecond,
			MaxRestartsInWindow: 10,
			RestartWindow:       10 * time.Minute,
		},
		logger: func(format string, v ...interface{}) {},
	}

	// Simulate: set restarting=true as if restartWithBackoff is sleeping
	m.mu.Lock()
	m.restarting = true
	m.mu.Unlock()

	// Multiple concurrent EnsureRunning calls should all return immediately
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := m.EnsureRunning()
			if err == nil {
				callCount.Add(1)
			}
		}()
	}
	wg.Wait()

	// All 5 should have returned nil (skipped because restarting=true)
	if got := callCount.Load(); got != 5 {
		t.Errorf("expected all 5 goroutines to return nil (skipped), got %d", got)
	}
}

func TestStartLocked_SkipsIfAlreadyRunning(t *testing.T) {
	// Verify that startLocked() re-checks isRunning() to close the TOCTOU window.
	// If the server is already running (m.process is alive), startLocked() should
	// return nil without attempting to start a second instance.
	tmpDir := t.TempDir()
	daemonDir := filepath.Join(tmpDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatal(err)
	}

	var logMessages []string
	m := &DoltServerManager{
		config: &DoltServerConfig{
			Enabled:  true,
			Port:     13307,
			Host:     "127.0.0.1",
			DataDir:  filepath.Join(tmpDir, "dolt"),
			LogFile:  filepath.Join(daemonDir, "dolt-server.log"),
		},
		townRoot: tmpDir,
		logger: func(format string, v ...interface{}) {
			logMessages = append(logMessages, fmt.Sprintf(format, v...))
		},
	}

	// Set m.process to our own process so isRunning() returns true.
	// Our own process is always alive.
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	m.process = self

	// Call startLocked() with the mutex held (as the contract requires).
	m.mu.Lock()
	err = m.startLocked()
	m.mu.Unlock()

	if err != nil {
		t.Fatalf("expected nil error when server already running, got: %v", err)
	}

	// Verify the skip was logged
	found := false
	for _, msg := range logMessages {
		if msg == "Dolt server already running, skipping start" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected 'already running' log message, got: %v", logMessages)
	}
}

func TestRestartWithBackoff_SkipsIfStartedDuringSleep(t *testing.T) {
	// Verify that restartWithBackoff() re-checks isRunning() after the backoff
	// sleep to detect if another goroutine started the server during the window.
	tmpDir := t.TempDir()
	daemonDir := filepath.Join(tmpDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatal(err)
	}

	var logMessages []string
	m := &DoltServerManager{
		config: &DoltServerConfig{
			Enabled:             true,
			Port:                13308,
			Host:                "127.0.0.1",
			DataDir:             filepath.Join(tmpDir, "dolt"),
			LogFile:             filepath.Join(daemonDir, "dolt-server.log"),
			RestartDelay:        10 * time.Millisecond, // Short delay for testing
			MaxRestartDelay:     100 * time.Millisecond,
			MaxRestartsInWindow: 10,
			RestartWindow:       10 * time.Minute,
		},
		townRoot: tmpDir,
		logger: func(format string, v ...interface{}) {
			logMessages = append(logMessages, fmt.Sprintf(format, v...))
		},
	}

	// Simulate: during backoff sleep, another goroutine starts the server.
	// We do this by launching restartWithBackoff in a goroutine and setting
	// m.process while it's sleeping.
	m.mu.Lock()

	done := make(chan error, 1)
	go func() {
		// restartWithBackoff expects to be called with m.mu held
		done <- m.restartWithBackoff()
	}()

	// Wait for the goroutine to release the lock during sleep
	time.Sleep(5 * time.Millisecond)

	// Simulate another goroutine starting the server by setting m.process
	m.mu.Lock()
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	m.process = self
	m.mu.Unlock()

	// Wait for restartWithBackoff to complete
	err = <-done

	if err != nil {
		t.Fatalf("expected nil error when server started during backoff, got: %v", err)
	}

	// Verify the skip was logged
	found := false
	for _, msg := range logMessages {
		if msg == "Dolt server started by another goroutine during backoff, skipping" ||
			msg == "Dolt server already running, skipping start" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected skip log message, got: %v", logMessages)
	}
}

func TestWriteAndClearUnhealthySignal(t *testing.T) {
	tmpDir := t.TempDir()
	daemonDir := filepath.Join(tmpDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatal(err)
	}

	m := &DoltServerManager{
		config:   DefaultDoltServerConfig(tmpDir),
		townRoot: tmpDir,
		logger:   func(format string, v ...interface{}) {},
	}

	// Initially no signal
	if IsDoltUnhealthy(tmpDir) {
		t.Error("expected no unhealthy signal initially")
	}

	// Write signal
	m.writeUnhealthySignal("server_dead", "PID 12345 is dead")

	if !IsDoltUnhealthy(tmpDir) {
		t.Error("expected unhealthy signal after write")
	}

	// Verify signal file contains JSON
	data, err := os.ReadFile(m.unhealthySignalFile())
	if err != nil {
		t.Fatalf("failed to read signal file: %v", err)
	}
	content := string(data)
	if content == "" {
		t.Error("signal file should not be empty")
	}

	// Clear signal
	m.clearUnhealthySignal()

	if IsDoltUnhealthy(tmpDir) {
		t.Error("expected no unhealthy signal after clear")
	}
}

func TestUnhealthySignalFile_Path(t *testing.T) {
	m := &DoltServerManager{
		config:   DefaultDoltServerConfig("/tmp/test-town"),
		townRoot: "/tmp/test-town",
		logger:   func(format string, v ...interface{}) {},
	}

	expected := "/tmp/test-town/daemon/DOLT_UNHEALTHY"
	if got := m.unhealthySignalFile(); got != expected {
		t.Errorf("expected %s, got %s", expected, got)
	}
}

func TestIsDoltUnhealthy_NoDir(t *testing.T) {
	// Non-existent directory should return false
	if IsDoltUnhealthy("/nonexistent/path") {
		t.Error("expected false for non-existent directory")
	}
}
