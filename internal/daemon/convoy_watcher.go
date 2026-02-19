package daemon

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// convoyPollInterval is how often the convoy watcher checks for completed convoys.
// Polling replaces the previous event-driven approach (bd activity --follow) which
// is no longer available in the current bd CLI. (gt-kn9a)
const convoyPollInterval = 30 * time.Second

// ConvoyWatcher monitors convoy completion by periodically running gt convoy check.
// It replaces the previous event-driven approach that subscribed to bd activity events.
type ConvoyWatcher struct {
	townRoot string
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	logger   func(format string, args ...interface{})

	// PATCH-006: Resolved binary paths to avoid PATH issues in subprocesses.
	gtPath string
}

// NewConvoyWatcher creates a new convoy watcher.
// PATCH-006: Added gtPath parameter.
func NewConvoyWatcher(townRoot string, logger func(format string, args ...interface{}), gtPath string) *ConvoyWatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &ConvoyWatcher{
		townRoot: townRoot,
		ctx:      ctx,
		cancel:   cancel,
		logger:   logger,
		gtPath:   gtPath,
	}
}

// Start begins the convoy watcher goroutine.
func (w *ConvoyWatcher) Start() error {
	w.wg.Add(1)
	go w.run()
	return nil
}

// Stop gracefully stops the convoy watcher.
func (w *ConvoyWatcher) Stop() {
	w.cancel()
	w.wg.Wait()
}

// run polls for convoy completion on a fixed interval.
func (w *ConvoyWatcher) run() {
	defer w.wg.Done()

	ticker := time.NewTicker(convoyPollInterval)
	defer ticker.Stop()

	// Run once immediately on start.
	w.checkAll()

	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			w.checkAll()
		}
	}
}

// checkAll runs gt convoy check to close any convoys whose tracked issues are all done.
func (w *ConvoyWatcher) checkAll() {
	cmd := exec.CommandContext(w.ctx, w.gtPath, "convoy", "check")
	cmd.Dir = w.townRoot
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// context.Canceled means we're shutting down — not an error worth logging.
		if w.ctx.Err() != nil {
			return
		}
		w.logger("convoy watcher: gt convoy check failed: %v: %s", err, stderr.String())
		return
	}

	if output := strings.TrimSpace(stdout.String()); output != "" && !strings.Contains(output, "No convoys ready") {
		w.logger("convoy watcher: %s", output)
	}
}
