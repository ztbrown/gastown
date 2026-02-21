// Package refinery provides the merge queue processing daemon.
package refinery

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/protocol"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/witness"
)

// GoDaemon is the Go-native refinery merge pipeline daemon.
// It replaces the LLM agent with deterministic merge processing.
// Event-driven: wakes on SIGUSR1 (sent by witness nudge), falls back to
// periodic polling as a safety net.
type GoDaemon struct {
	rig     *rig.Rig
	eng     *Engineer
	mailbox *mail.Mailbox
	router  *mail.Router
	handler *protocol.DefaultRefineryHandler
	logger  *log.Logger
	wakeup  chan struct{}
	output  io.Writer
}

// pollInterval is the safety-net poll interval when idle.
// SIGUSR1 provides immediate wakeup for event-driven operation.
const pollInterval = 30 * time.Second

// NewGoDaemon creates a new Go refinery daemon for the given rig.
func NewGoDaemon(r *rig.Rig) *GoDaemon {
	identity := fmt.Sprintf("%s/refinery", r.Name)
	mb := mail.NewMailboxBeads(identity, r.Path)

	logger := log.New(os.Stderr, fmt.Sprintf("[refinery/%s] ", r.Name), log.LstdFlags)

	return &GoDaemon{
		rig:     r,
		eng:     NewEngineer(r),
		mailbox: mb,
		router:  mail.NewRouter(r.Path),
		handler: protocol.NewRefineryHandler(r.Name, r.Path),
		logger:  logger,
		wakeup:  make(chan struct{}, 1),
		output:  os.Stdout,
	}
}

// SetOutput sets the output writer for status messages.
func (d *GoDaemon) SetOutput(w io.Writer) {
	d.output = w
	d.eng.SetOutput(w)
}

// PidFile returns the path to the daemon's PID file.
func (d *GoDaemon) PidFile() string {
	return filepath.Join(d.rig.Path, "refinery", "refinery.pid")
}

// Run starts the daemon event loop. It blocks until ctx is cancelled.
// Writes a PID file on startup and removes it on exit.
func (d *GoDaemon) Run(ctx context.Context) error {
	// Load merge queue config
	if err := d.eng.LoadConfig(); err != nil {
		d.logger.Printf("Warning: failed to load merge queue config: %v (using defaults)", err)
	}

	// Ensure PID directory exists
	pidDir := filepath.Dir(d.PidFile())
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		return fmt.Errorf("creating pid directory: %w", err)
	}

	// Write PID file
	if err := os.WriteFile(d.PidFile(), []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		return fmt.Errorf("writing PID file: %w", err)
	}
	defer func() { _ = os.Remove(d.PidFile()) }()

	d.logger.Printf("Refinery daemon started (PID %d) for rig %s", os.Getpid(), d.rig.Name)
	_, _ = fmt.Fprintf(d.output, "[Refinery] Go daemon started for rig %s\n", d.rig.Name)

	// Set up signal handling for graceful shutdown and wakeup
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, refinerySignals()...)
	defer signal.Stop(sigCh)

	// Initial scan on startup (process any MRs that arrived while down)
	d.processCycle(ctx)

	timer := time.NewTimer(pollInterval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			d.logger.Printf("Refinery daemon shutting down")
			return nil

		case sig := <-sigCh:
			if isWakeupSignal(sig) {
				// Wakeup signal from witness nudge
				d.logger.Printf("Wakeup signal received — processing merge queue")
				d.processCycle(ctx)
				// Reset timer after processing
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(pollInterval)
			} else {
				d.logger.Printf("Shutdown signal received (%v)", sig)
				return nil
			}

		case <-d.wakeup:
			// Internal wakeup (future use)
			d.processCycle(ctx)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(pollInterval)

		case <-timer.C:
			// Safety-net poll
			d.processCycle(ctx)
			timer.Reset(pollInterval)
		}
	}
}

// processCycle reads the inbox and processes all ready merge requests.
func (d *GoDaemon) processCycle(ctx context.Context) {
	// Drain and handle inbox messages first
	d.drainInbox(ctx)

	// Process all ready MRs (may have become ready since last wakeup)
	d.processReadyMRs(ctx)
}

// drainInbox reads all unread messages and handles them.
func (d *GoDaemon) drainInbox(_ context.Context) {
	msgs, err := d.mailbox.ListUnread()
	if err != nil {
		d.logger.Printf("Warning: listing inbox: %v", err)
		return
	}

	for _, msg := range msgs {
		d.handleMessage(msg)
	}
}

// handleMessage dispatches a single inbox message.
func (d *GoDaemon) handleMessage(msg *mail.Message) {
	proto := witness.ClassifyMessage(msg.Subject)

	switch proto {
	case witness.ProtoMergeReady:
		// Acknowledge receipt; processReadyMRs handles actual work
		d.logger.Printf("MERGE_READY from %s: %s", msg.From, msg.Subject)
		_, _ = fmt.Fprintf(d.output, "[Refinery] MERGE_READY received — will process queue\n")

	default:
		// Forward unknown messages to Mayor
		d.logger.Printf("Unknown message type %q (subject=%q) — forwarding to mayor", proto, msg.Subject)
		d.forwardToMayor(msg)
	}

	// Mark as read regardless of handling outcome
	if err := d.mailbox.MarkRead(msg.ID); err != nil {
		d.logger.Printf("Warning: marking message %s read: %v", msg.ID, err)
	}
}

// forwardToMayor forwards an unrecognized message to the Mayor.
func (d *GoDaemon) forwardToMayor(msg *mail.Message) {
	fwd := mail.NewMessage(
		fmt.Sprintf("%s/refinery", d.rig.Name),
		"mayor/",
		fmt.Sprintf("[FWD from refinery] %s", msg.Subject),
		fmt.Sprintf("Forwarded from %s/refinery (unrecognized message type):\n\nFrom: %s\nSubject: %s\n\n%s",
			d.rig.Name, msg.From, msg.Subject, msg.Body),
	)
	if err := d.router.Send(fwd); err != nil {
		d.logger.Printf("Warning: forwarding to mayor: %v", err)
	}
}

// processReadyMRs processes all ready merge requests in priority order.
func (d *GoDaemon) processReadyMRs(ctx context.Context) {
	mrs, err := d.eng.ListReadyMRs()
	if err != nil {
		d.logger.Printf("Warning: listing ready MRs: %v", err)
		return
	}

	if len(mrs) == 0 {
		return
	}

	d.logger.Printf("Processing %d ready MR(s)", len(mrs))

	for _, mr := range mrs {
		if ctx.Err() != nil {
			return // Context cancelled
		}
		d.processMR(ctx, mr)
	}
}

// processMR processes a single merge request through the pipeline.
func (d *GoDaemon) processMR(ctx context.Context, mr *MRInfo) {
	_, _ = fmt.Fprintf(d.output, "[Refinery] Processing MR %s (branch: %s → %s)\n",
		mr.ID, mr.Branch, mr.Target)

	result := d.eng.ProcessMRInfo(ctx, mr)

	if result.Success {
		d.eng.HandleMRInfoSuccess(mr, result)
		// Send MERGED notification to Witness
		if err := d.handler.SendMerged(mr.Worker, mr.Branch, mr.SourceIssue, mr.Target, result.MergeCommit); err != nil {
			d.logger.Printf("Warning: sending MERGED for %s: %v", mr.ID, err)
		} else {
			d.logger.Printf("Merged %s (commit: %s)", mr.ID, shortSHA(result.MergeCommit))
		}
	} else {
		d.eng.HandleMRInfoFailure(mr, result)
		// HandleMRInfoFailure already sends MERGE_FAILED to Witness
		d.logger.Printf("Failed to merge %s: %s", mr.ID, result.Error)
	}
}

// shortSHA returns the first 8 chars of a SHA, or the full SHA if shorter.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// RefineryDaemonPidFile returns the PID file path for a given rig.
// Used by Manager.IsRunning() and nudgeRefinery() without creating a full daemon.
func RefineryDaemonPidFile(rigPath string) string {
	return filepath.Join(rigPath, "refinery", "refinery.pid")
}

// ReadRefineryDaemonPID reads the PID from the refinery daemon PID file.
// Returns 0 and nil error if the file does not exist.
func ReadRefineryDaemonPID(rigPath string) (int, error) {
	pidFile := RefineryDaemonPidFile(rigPath)
	data, err := os.ReadFile(pidFile)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading refinery PID file: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parsing refinery PID: %w", err)
	}
	return pid, nil
}

// IsRefineryDaemonRunning checks whether the refinery Go daemon is running
// by checking the PID file and verifying the process is alive.
func IsRefineryDaemonRunning(rigPath string) (bool, int, error) {
	pid, err := ReadRefineryDaemonPID(rigPath)
	if err != nil {
		return false, 0, err
	}
	if pid == 0 {
		return false, 0, nil
	}

	// Check if process is alive
	proc, err := os.FindProcess(pid)
	if err != nil {
		// Process not found — stale PID file
		_ = os.Remove(RefineryDaemonPidFile(rigPath))
		return false, 0, nil
	}

	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Process not running — stale PID file
		_ = os.Remove(RefineryDaemonPidFile(rigPath))
		return false, 0, nil
	}

	return true, pid, nil
}
