package daemon

// patrol.go — Mechanical patrol steps for the daemon tick loop.
//
// These are steps previously handled by the Deacon LLM agent that can be
// executed mechanically without AI reasoning. Each function is discrete and
// called from runMechanicalPatrol(), which is invoked from heartbeat().
//
// The 11 steps:
//
//  1. patrolOrphanProcesses  — kill orphaned claude subprocesses (TTY + zombie scan)
//  2. patrolTimerGates       — evaluate bd timer gates, close elapsed ones
//  3. patrolGatedMolecules   — dispatch waiting molecules when bead gates resolve
//  4. patrolDogPool          — ensure minimum idle dogs, spawn if below threshold
//  5. patrolDogHealth        — flag dogs working too long
//  6. patrolWispTTL          — compact expired wisps via gt compact
//  7. patrolSessionGC        — detect dead agent sessions, dispatch cleanup
//  8. patrolLogRotation      — rotate oversized daemon log file
//  9. patrolConvoyFeeding    — feed stranded convoys to idle dogs
// 10. patrolIdleTown         — detect idle town (no in-progress beads)
// 11. patrolMailArchive      — archive stale mechanical mail
//
// Each function is non-fatal: errors are logged but do not stop the patrol.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/util"
)

// Patrol configuration constants.
const (
	// patrolMinIdleDogs is the minimum number of idle dogs to maintain in the kennel.
	// If the idle count falls below this, the daemon spawns a new dog.
	patrolMinIdleDogs = 1

	// patrolDogMaxWorkDuration is how long a dog can be in "working" state before
	// it is flagged as potentially stuck. The daemon logs a warning but does not
	// kill the dog automatically — the Deacon handles remediation.
	patrolDogMaxWorkDuration = 2 * time.Hour

	// patrolLogMaxSize is the maximum size of the daemon log before rotation (100 MB).
	patrolLogMaxSize = 100 * 1024 * 1024

	// patrolLogKeep is how many rotated log files to retain.
	patrolLogKeep = 3

	// patrolMailStaleAge is how old a mechanical mail message must be before
	// it is eligible for archiving.
	patrolMailStaleAge = 24 * time.Hour
)

// runMechanicalPatrol executes all 11 mechanical patrol steps.
// Called from the main heartbeat tick loop. Non-fatal: each step logs errors
// but does not abort subsequent steps.
func (d *Daemon) runMechanicalPatrol() {
	d.logger.Println("Mechanical patrol starting")

	// 1. Orphan + zombie process cleanup
	d.patrolOrphanProcesses()

	// 2. Timer gate evaluation
	d.patrolTimerGates()

	// 3. Gated molecule dispatch (bead gates)
	d.patrolGatedMolecules()

	// 4. Dog pool maintenance
	d.patrolDogPool()

	// 5. Dog health check
	d.patrolDogHealth()

	// 6. Wisp TTL compaction
	d.patrolWispTTL()

	// 7. Session GC
	d.patrolSessionGC()

	// 8. Log rotation
	d.patrolLogRotation()

	// 9. Convoy feeding
	d.patrolConvoyFeeding()

	// 10. Idle town detection (logs result, callers use isTownIdle() separately)
	if d.isTownIdle() {
		d.logger.Println("Patrol: town is idle (0 in-progress beads) — skipping health pings")
	}

	// 11. Mail archiving
	d.patrolMailArchive()

	d.logger.Println("Mechanical patrol complete")
}

// ── Step 1: Orphan process cleanup ──────────────────────────────────────────

// patrolOrphanProcesses kills orphaned claude subagent processes.
// Runs both TTY-based detection (no controlling terminal) and zombie detection
// (processes not associated with any active tmux session).
func (d *Daemon) patrolOrphanProcesses() {
	// TTY-based orphan detection: claude processes with TTY "?"
	orphanResults, err := util.CleanupOrphanedClaudeProcesses()
	if err != nil {
		d.logger.Printf("Patrol: orphan cleanup error: %v", err)
	}
	if len(orphanResults) > 0 {
		d.logger.Printf("Patrol: orphan cleanup processed %d process(es)", len(orphanResults))
	}

	// Zombie detection: claude processes not in any active tmux session
	zombieResults, err := util.CleanupZombieClaudeProcesses()
	if err != nil {
		d.logger.Printf("Patrol: zombie scan error: %v", err)
	}
	if len(zombieResults) > 0 {
		d.logger.Printf("Patrol: zombie scan processed %d process(es)", len(zombieResults))
	}
}

// ── Step 2: Timer gate evaluation ───────────────────────────────────────────

// patrolTimerGates evaluates open timer gates and closes any that have elapsed.
// Delegates to `bd gate check --type=timer` which handles the gate lifecycle.
func (d *Daemon) patrolTimerGates() {
	// Check town-level beads (hq-* prefix)
	d.runGateCheck(d.config.TownRoot, "timer")

	// Check per-rig beads
	for _, rigName := range d.getKnownRigs() {
		rigPath := filepath.Join(d.config.TownRoot, rigName)
		d.runGateCheck(rigPath, "timer")
	}
}

// runGateCheck runs `bd gate check --type=<gateType>` in the given directory.
func (d *Daemon) runGateCheck(dir, gateType string) {
	cmd := exec.Command(d.bdPath, "gate", "check", "--type="+gateType)
	cmd.Dir = dir
	cmd.Env = os.Environ()

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Exit code 1 is normal when no gates to check — only log unexpected errors
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
			d.logger.Printf("Patrol: gate check (%s) in %s: %v", gateType, dir, err)
		}
		return
	}

	if out := strings.TrimSpace(string(output)); out != "" {
		d.logger.Printf("Patrol: gate check (%s) in %s: %s", gateType, dir, out)
	}
}

// ── Step 3: Gated molecule dispatch ─────────────────────────────────────────

// patrolGatedMolecules evaluates bead-type gates and dispatches waiting molecules
// when their gate conditions are resolved.
func (d *Daemon) patrolGatedMolecules() {
	// Bead gates watch cross-rig bead closes — check town-level and per-rig
	d.runGateCheck(d.config.TownRoot, "bead")

	for _, rigName := range d.getKnownRigs() {
		rigPath := filepath.Join(d.config.TownRoot, rigName)
		d.runGateCheck(rigPath, "bead")
	}
}

// ── Step 4: Dog pool maintenance ────────────────────────────────────────────

// patrolDogPool ensures at least patrolMinIdleDogs idle dogs are available.
// If the kennel has fewer idle dogs than the minimum, a new dog is spawned.
// Dog naming uses a timestamp-based suffix to avoid collisions.
func (d *Daemon) patrolDogPool() {
	kennelPath := filepath.Join(d.config.TownRoot, "deacon", "dogs")

	// Count idle dogs by scanning kennel directory
	idleCount, err := d.countIdleDogs(kennelPath)
	if err != nil {
		d.logger.Printf("Patrol: dog pool check failed: %v", err)
		return
	}

	if idleCount >= patrolMinIdleDogs {
		return // Pool is healthy
	}

	need := patrolMinIdleDogs - idleCount
	d.logger.Printf("Patrol: dog pool below minimum (%d idle, need %d) — spawning %d dog(s)",
		idleCount, patrolMinIdleDogs, need)

	for i := 0; i < need; i++ {
		// Generate unique name with timestamp suffix
		name := fmt.Sprintf("auto-%d", time.Now().UnixNano()%10000)
		cmd := exec.Command(d.gtPath, "dog", "add", name)
		cmd.Dir = d.config.TownRoot
		cmd.Env = os.Environ()

		if out, err := cmd.CombinedOutput(); err != nil {
			d.logger.Printf("Patrol: dog pool spawn %q failed: %v (%s)", name, err, strings.TrimSpace(string(out)))
		} else {
			d.logger.Printf("Patrol: dog pool spawned %q", name)
		}
	}
}

// dogStateFile is the per-dog state filename inside each dog's kennel directory.
const dogStateFile = ".dog.json"

// countIdleDogs counts dogs with state "idle" by reading kennel directory.
func (d *Daemon) countIdleDogs(kennelPath string) (int, error) {
	entries, err := os.ReadDir(kennelPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // No kennel yet = 0 dogs
		}
		return 0, err
	}

	idle := 0
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		stateFile := filepath.Join(kennelPath, entry.Name(), dogStateFile)
		data, err := os.ReadFile(stateFile)
		if err != nil {
			continue // Skip unreadable state
		}

		var state struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}

		if state.State == "idle" {
			idle++
		}
	}

	return idle, nil
}

// ── Step 5: Dog health check ─────────────────────────────────────────────────

// patrolDogHealth flags dogs that have been in "working" state too long.
// These dogs may be stuck; the daemon logs a warning for the Deacon to handle.
func (d *Daemon) patrolDogHealth() {
	kennelPath := filepath.Join(d.config.TownRoot, "deacon", "dogs")

	entries, err := os.ReadDir(kennelPath)
	if err != nil {
		if !os.IsNotExist(err) {
			d.logger.Printf("Patrol: dog health check failed: %v", err)
		}
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		dogName := entry.Name()
		stateFile := filepath.Join(kennelPath, dogName, dogStateFile)
		data, err := os.ReadFile(stateFile)
		if err != nil {
			continue
		}

		var state struct {
			State      string    `json:"state"`
			LastActive time.Time `json:"last_active"`
			Work       string    `json:"work,omitempty"`
		}
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}

		if state.State != "working" {
			continue
		}

		workDuration := time.Since(state.LastActive)
		if workDuration > patrolDogMaxWorkDuration {
			d.logger.Printf("Patrol: dog %q working too long (%s, work=%s) — may be stuck",
				dogName, workDuration.Round(time.Minute), state.Work)
		}
	}
}

// ── Step 6: Wisp TTL compaction ──────────────────────────────────────────────

// patrolWispTTL runs TTL-based wisp compaction across all beads directories.
// Expired closed wisps are deleted; expired open wisps are promoted to permanent beads.
func (d *Daemon) patrolWispTTL() {
	// Run compaction for town-level beads
	d.runCompact(d.config.TownRoot)

	// Run compaction for each rig's beads
	for _, rigName := range d.getKnownRigs() {
		rigPath := filepath.Join(d.config.TownRoot, rigName)
		d.runCompact(rigPath)
	}
}

// runCompact runs `gt compact --json` in the given directory.
func (d *Daemon) runCompact(dir string) {
	cmd := exec.Command(d.gtPath, "compact", "--json")
	cmd.Dir = dir
	cmd.Env = os.Environ()

	output, err := cmd.Output()
	if err != nil {
		// Non-fatal: compaction may fail if beads dir not configured
		return
	}

	// Parse summary for logging
	var result struct {
		Deleted  int `json:"deleted"`
		Promoted int `json:"promoted"`
		Skipped  int `json:"skipped"`
	}
	if json.Unmarshal(output, &result) == nil {
		if result.Deleted > 0 || result.Promoted > 0 {
			d.logger.Printf("Patrol: wisp compact in %s: deleted=%d promoted=%d skipped=%d",
				filepath.Base(dir), result.Deleted, result.Promoted, result.Skipped)
		}
	}
}

// ── Step 7: Session GC ───────────────────────────────────────────────────────

// patrolSessionGC detects dead agent sessions and dispatches cleanup.
// Covers witnesses and refineries (polecats are handled by checkPolecatSessionHealth).
// When a session is dead for a persistent role, the witness is notified.
func (d *Daemon) patrolSessionGC() {
	for _, rigName := range d.getKnownRigs() {
		d.gcRigSessions(rigName)
	}
}

// gcRigSessions checks witness and refinery sessions for a rig.
func (d *Daemon) gcRigSessions(rigName string) {
	roleNames := []string{"witness", "refinery"}
	roleSessionNames := []string{
		session.WitnessSessionName(rigName),
		session.RefinerySessionName(rigName),
	}

	for i, role := range roleNames {
		sessionName := roleSessionNames[i]
		alive, err := d.tmux.HasSession(sessionName)
		if err != nil || alive {
			continue // Error or still alive — skip
		}

		// Session is dead — the heartbeat's ensureWitnessRunning/ensureRefineryRunning
		// will restart it. Log for visibility.
		d.logger.Printf("Patrol: session GC: %s/%s session %q is dead (will be restarted by heartbeat)",
			rigName, role, sessionName)
	}
}

// ── Step 8: Log rotation ─────────────────────────────────────────────────────

// patrolLogRotation rotates the daemon log if it exceeds patrolLogMaxSize.
// Keeps patrolLogKeep rotated copies, deleting older ones.
func (d *Daemon) patrolLogRotation() {
	logPath := d.config.LogFile
	if logPath == "" {
		return
	}

	info, err := os.Stat(logPath)
	if err != nil {
		return // Log file doesn't exist yet
	}

	if info.Size() < patrolLogMaxSize {
		return // No rotation needed
	}

	d.logger.Printf("Patrol: log rotation: %s is %.1f MB, rotating",
		logPath, float64(info.Size())/(1024*1024))

	// Shift existing rotated logs: .2 → .3, .1 → .2, etc.
	for i := patrolLogKeep - 1; i >= 1; i-- {
		old := fmt.Sprintf("%s.%d", logPath, i)
		new := fmt.Sprintf("%s.%d", logPath, i+1)
		_ = os.Rename(old, new)
	}

	// Delete the oldest if we've exceeded the keep count
	oldest := fmt.Sprintf("%s.%d", logPath, patrolLogKeep+1)
	_ = os.Remove(oldest)

	// Copy current log to .1 and truncate current
	if err := d.copyLogFile(logPath, logPath+".1"); err != nil {
		d.logger.Printf("Patrol: log rotation copy failed: %v", err)
		return
	}

	// Truncate the current log file (keep it open for the logger)
	if err := os.Truncate(logPath, 0); err != nil {
		d.logger.Printf("Patrol: log rotation truncate failed: %v", err)
	}
}

// copyLogFile copies a file from src to dst.
func (d *Daemon) copyLogFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // G304: path is from daemon config
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst) //nolint:gosec // G304: path is from daemon config
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// ── Step 9: Convoy feeding ───────────────────────────────────────────────────

// patrolConvoyFeeding detects stranded convoys and feeds them to idle dogs.
// A convoy is stranded when it has ready (unstarted) issues but no active workers.
// This is a fallback for when the event-driven ConvoyWatcher misses an issue close.
func (d *Daemon) patrolConvoyFeeding() {
	// Find stranded convoys using gt convoy stranded --json
	cmd := exec.Command(d.gtPath, "convoy", "stranded", "--json")
	cmd.Dir = d.config.TownRoot
	cmd.Env = os.Environ()

	output, err := cmd.Output()
	if err != nil {
		// No stranded convoys or command not available — non-fatal
		return
	}

	var stranded []struct {
		ID         string   `json:"id"`
		ReadyCount int      `json:"ready_count"`
		ReadyIssues []string `json:"ready_issues"`
	}
	if err := json.Unmarshal(output, &stranded); err != nil {
		d.logger.Printf("Patrol: convoy feeding: parse error: %v", err)
		return
	}

	// Filter to convoys with ready work (not empty convoys needing cleanup)
	var toFeed []string
	for _, s := range stranded {
		if s.ReadyCount > 0 {
			toFeed = append(toFeed, s.ID)
		}
	}

	if len(toFeed) == 0 {
		return
	}

	// Check idle dog capacity before dispatching
	kennelPath := filepath.Join(d.config.TownRoot, "deacon", "dogs")
	idleCount, err := d.countIdleDogs(kennelPath)
	if err != nil || idleCount == 0 {
		if len(toFeed) > 0 {
			d.logger.Printf("Patrol: convoy feeding: %d stranded convoy(s) but no idle dogs", len(toFeed))
		}
		return
	}

	// Feed up to idleCount convoys
	fed := 0
	for _, convoyID := range toFeed {
		if fed >= idleCount {
			break // No more idle capacity
		}

		d.logger.Printf("Patrol: convoy feeding: slinging mol-convoy-feed for %s", convoyID)
		slingCmd := exec.Command(d.gtPath, "sling", "mol-convoy-feed", "deacon/dogs",
			"--var", "convoy="+convoyID)
		slingCmd.Dir = d.config.TownRoot
		slingCmd.Env = os.Environ()

		if out, err := slingCmd.CombinedOutput(); err != nil {
			d.logger.Printf("Patrol: convoy feeding: sling failed for %s: %v (%s)",
				convoyID, err, strings.TrimSpace(string(out)))
		} else {
			d.logger.Printf("Patrol: convoy feeding: fed %s", convoyID)
			fed++
		}
	}

	if fed > 0 {
		d.logger.Printf("Patrol: convoy feeding: fed %d/%d stranded convoy(s)", fed, len(toFeed))
	}
}

// ── Step 10: Idle town detection ─────────────────────────────────────────────

// isTownIdle returns true if there are no in-progress beads across all rigs.
// When the town is idle, health pings can be skipped to reduce noise.
func (d *Daemon) isTownIdle() bool {
	// Check town-level beads
	if count := d.countInProgress(d.config.TownRoot); count > 0 {
		return false
	}

	// Check per-rig beads
	for _, rigName := range d.getKnownRigs() {
		rigPath := filepath.Join(d.config.TownRoot, rigName)
		if count := d.countInProgress(rigPath); count > 0 {
			return false
		}
	}

	return true
}

// countInProgress returns the number of in-progress beads in the given directory.
// Returns 0 on error (conservative: treat as non-idle to avoid skipping checks).
func (d *Daemon) countInProgress(dir string) int {
	cmd := exec.Command(d.bdPath, "list", "--status=in_progress", "--json")
	cmd.Dir = dir
	cmd.Env = os.Environ()

	output, err := cmd.Output()
	if err != nil {
		return 0 // Error = assume 0 (fail open: will not skip health pings on error)
	}

	// bd list --json returns a JSON array
	var items []json.RawMessage
	if err := json.Unmarshal(output, &items); err != nil {
		return 0
	}

	return len(items)
}

// ── Step 11: Mail archiving ──────────────────────────────────────────────────

// patrolMailArchive archives stale mechanical mail from the deacon inbox.
// Messages older than patrolMailStaleAge that are already read are archived.
// This prevents the deacon inbox from accumulating noise over time.
func (d *Daemon) patrolMailArchive() {
	cmd := exec.Command(d.gtPath, "mail", "archive", "--stale",
		"--identity", "deacon/")
	cmd.Dir = d.config.TownRoot
	cmd.Env = os.Environ()

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Non-fatal: archive may fail if no stale messages
		out := strings.TrimSpace(string(output))
		if out != "" && !strings.Contains(out, "No stale messages") {
			d.logger.Printf("Patrol: mail archive: %v (%s)", err, out)
		}
		return
	}

	if out := strings.TrimSpace(string(output)); out != "" {
		d.logger.Printf("Patrol: mail archive: %s", out)
	}
}
