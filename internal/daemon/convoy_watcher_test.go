package daemon

import (
	"testing"
	"time"
)

func TestConvoyWatcherStartStop(t *testing.T) {
	// Verify the watcher starts and stops cleanly without errors.
	// Uses a nonexistent binary so checkAll() fails but does not panic.
	w := NewConvoyWatcher(t.TempDir(), func(format string, args ...interface{}) {}, "/nonexistent-gt")
	if err := w.Start(); err != nil {
		t.Fatalf("Start() returned error: %v", err)
	}
	// Give the initial checkAll() a moment to run before stopping.
	time.Sleep(10 * time.Millisecond)
	w.Stop()
}
