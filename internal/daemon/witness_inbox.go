package daemon

// witness_inbox.go — Daemon-side witness inbox processing.
//
// Processes witness inboxes directly in the Go daemon tick loop.
// This replaces the previous flow where the daemon kept an LLM witness session
// alive to process inbox messages.
//
// Old flow: daemon ensures witness session alive → LLM witness reads inbox → LLM acts
// New flow: daemon reads witness inbox → routes to Go handlers → marks messages read
//           unrecognized messages → escalated to Mayor
//
// Protocol messages handled:
//   POLECAT_DONE <name>        → witness.HandlePolecatDone
//   LIFECYCLE:Shutdown <name>  → witness.HandleLifecycleShutdown
//   HELP: <topic>              → witness.HandleHelp (escalates to Mayor)
//   MERGED <name>              → witness.HandleMerged
//   MERGE_FAILED <name>        → witness.HandleMergeFailed
//   SWARM_START                → witness.HandleSwarmStart
//   🤝 HANDOFF                 → discarded (session continuity, not actionable)
//   MERGE_READY <name>         → discarded (outbound-only protocol message)
//   <unrecognized>             → escalated to Mayor, left unread for inspection

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/witness"
)

// processWitnessInboxes reads and processes witness inboxes for all active rigs.
// Called from heartbeat() on each daemon tick.
// Replaces forwardWitnessEscalations (which only handled HELP and MERGE_FAILED).
func (d *Daemon) processWitnessInboxes() {
	if d.escalator == nil {
		return
	}
	for _, rigName := range d.getKnownRigs() {
		d.processRigWitnessInbox(rigName)
	}
}

// processRigWitnessInbox reads and processes the witness inbox for one rig.
// Messages are closed (deleted) after successful handling to prevent re-processing.
// Unrecognized messages are escalated to Mayor and left unread for manual inspection.
func (d *Daemon) processRigWitnessInbox(rigName string) {
	witnessIdentity := rigName + "/witness"
	// Read witness inbox as JSON
	cmd := exec.Command(d.gtPath, "mail", "inbox", "--identity", witnessIdentity, "--json") //nolint:gosec // G204
	cmd.Dir = d.config.TownRoot
	cmd.Env = os.Environ()

	output, err := cmd.Output()
	if err != nil {
		return // Non-fatal: inbox may be empty or witness not yet configured
	}
	if len(output) == 0 || string(output) == "[]" || string(output) == "[]\n" {
		return
	}

	var messages []BeadsMessage
	if err := json.Unmarshal(output, &messages); err != nil {
		d.logger.Printf("Witness inbox [%s]: parse error: %v", rigName, err)
		return
	}

	workDir := filepath.Join(d.config.TownRoot, rigName)
	router := mail.NewRouterWithTownRoot(workDir, d.config.TownRoot)

	processed := 0
	for i := range messages {
		msg := &messages[i]
		if msg.Read {
			continue // Already handled (closed beads message = read)
		}

		handled := d.handleWitnessMessage(rigName, workDir, router, msg)
		if handled {
			// Close the message to prevent re-processing on the next tick.
			// This is the idempotency mechanism: closed = processed.
			if err := d.closeMessage(msg.ID); err != nil {
				d.logger.Printf("Witness inbox [%s]: failed to close message %s: %v", rigName, msg.ID, err)
			} else {
				processed++
			}
		}
	}

	if processed > 0 {
		d.logger.Printf("Witness inbox [%s]: processed %d message(s)", rigName, processed)
	}
}

// handleWitnessMessage routes a witness inbox message to the appropriate handler.
// Returns true if the message was handled and should be marked as read/closed.
// Returns false if the message should be left unread for manual inspection.
func (d *Daemon) handleWitnessMessage(rigName, workDir string, router *mail.Router, msg *BeadsMessage) bool {
	protoType := witness.ClassifyMessage(msg.Subject)
	mailMsg := witnessBeadsToMail(msg)

	switch protoType {
	case witness.ProtoPolecatDone:
		result := witness.HandlePolecatDone(workDir, rigName, mailMsg, router)
		d.logWitnessResult(rigName, "POLECAT_DONE", msg.ID, result)
		return true // Always close recognized messages after attempt

	case witness.ProtoLifecycleShutdown:
		result := witness.HandleLifecycleShutdown(workDir, rigName, mailMsg)
		d.logWitnessResult(rigName, "LIFECYCLE:Shutdown", msg.ID, result)
		return true

	case witness.ProtoHelp:
		result := witness.HandleHelp(workDir, rigName, mailMsg, router)
		d.logWitnessResult(rigName, "HELP", msg.ID, result)
		return true

	case witness.ProtoMerged:
		result := witness.HandleMerged(workDir, rigName, mailMsg)
		d.logWitnessResult(rigName, "MERGED", msg.ID, result)
		return true

	case witness.ProtoMergeFailed:
		result := witness.HandleMergeFailed(workDir, rigName, mailMsg, router)
		d.logWitnessResult(rigName, "MERGE_FAILED", msg.ID, result)
		return true

	case witness.ProtoSwarmStart:
		result := witness.HandleSwarmStart(workDir, mailMsg)
		d.logWitnessResult(rigName, "SWARM_START", msg.ID, result)
		return true

	case witness.ProtoHandoff:
		// HANDOFF messages are for witness LLM session continuity.
		// In the daemon-driven flow they are not actionable; discard silently.
		d.logger.Printf("Witness inbox [%s]: discarding HANDOFF message %s", rigName, msg.ID)
		return true

	case witness.ProtoMergeReady:
		// MERGE_READY is outbound from witness to refinery.
		// Should not appear in the witness inbox; discard.
		d.logger.Printf("Witness inbox [%s]: unexpected MERGE_READY in inbox, discarding %s (subject: %q)",
			rigName, msg.ID, msg.Subject)
		return true

	default:
		// Unrecognized message type — escalate to Mayor and leave unread for inspection.
		// The escalator's 30-minute dedup prevents repeated escalation on each tick.
		d.logger.Printf("Witness inbox [%s]: unrecognized message %s (subject: %q), escalating to Mayor",
			rigName, msg.ID, msg.Subject)
		d.escalateUnrecognizedWitnessMessage(rigName, msg)
		return false // Leave in inbox for manual inspection
	}
}

// logWitnessResult logs the outcome of a witness handler call.
func (d *Daemon) logWitnessResult(rigName, msgType, msgID string, result *witness.HandlerResult) {
	if result == nil {
		return
	}
	if result.Error != nil {
		d.logger.Printf("Witness inbox [%s]: %s [%s] error: %v (action: %s)",
			rigName, msgType, msgID, result.Error, result.Action)
	} else if result.Action != "" {
		d.logger.Printf("Witness inbox [%s]: %s [%s] → %s",
			rigName, msgType, msgID, result.Action)
	}
}

// escalateUnrecognizedWitnessMessage escalates an unrecognized witness inbox message to Mayor.
// The escalator deduplicates per rig to avoid flooding Mayor on repeated ticks.
func (d *Daemon) escalateUnrecognizedWitnessMessage(rigName string, msg *BeadsMessage) {
	if d.escalator == nil {
		return
	}
	d.escalator.EscalateToMayor(EscalationContext{
		Kind:         KindHelpRequest,
		Priority:     "high",
		Rig:          rigName,
		HelpTopic:    "unrecognized witness inbox message",
		HelpBody:     fmt.Sprintf("From: %s\nSubject: %s\n\n%s", msg.From, msg.Subject, msg.Body),
		HelpAgentID:  fmt.Sprintf("%s/witness", rigName),
		ErrorDetails: fmt.Sprintf("message_id=%s subject=%q", msg.ID, msg.Subject),
	})
}

// witnessBeadsToMail converts a daemon BeadsMessage to a mail.Message
// for use with witness protocol handlers that expect the mail.Message type.
func witnessBeadsToMail(msg *BeadsMessage) *mail.Message {
	out := &mail.Message{
		ID:       msg.ID,
		From:     msg.From,
		To:       msg.To,
		Subject:  msg.Subject,
		Body:     msg.Body,
		Read:     msg.Read,
		Priority: mail.PriorityNormal,
		Type:     mail.TypeNotification,
	}

	// Parse timestamp (RFC3339 from gt mail inbox --json).
	// Fall back to current time if unparseable.
	if ts, err := time.Parse(time.RFC3339, msg.Timestamp); err == nil {
		out.Timestamp = ts
	} else {
		out.Timestamp = time.Now()
	}

	// Map priority strings to typed constants.
	switch msg.Priority {
	case "urgent":
		out.Priority = mail.PriorityUrgent
	case "high":
		out.Priority = mail.PriorityHigh
	case "low":
		out.Priority = mail.PriorityLow
	}

	// Map message type strings to typed constants.
	switch msg.Type {
	case "task":
		out.Type = mail.TypeTask
	case "scavenge":
		out.Type = mail.TypeScavenge
	case "reply":
		out.Type = mail.TypeReply
	}

	return out
}
