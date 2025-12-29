package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/tmux"
)

var (
	statusLineSession string
)

var statusLineCmd = &cobra.Command{
	Use:    "status-line",
	Short:  "Output status line content for tmux (internal use)",
	Hidden: true, // Internal command called by tmux
	RunE:   runStatusLine,
}

func init() {
	rootCmd.AddCommand(statusLineCmd)
	statusLineCmd.Flags().StringVar(&statusLineSession, "session", "", "Tmux session name")
}

func runStatusLine(cmd *cobra.Command, args []string) error {
	t := tmux.NewTmux()

	// Get session environment
	var rigName, polecat, crew, issue, role string

	if statusLineSession != "" {
		// Non-fatal: missing env vars are handled gracefully below
		rigName, _ = t.GetEnvironment(statusLineSession, "GT_RIG")
		polecat, _ = t.GetEnvironment(statusLineSession, "GT_POLECAT")
		crew, _ = t.GetEnvironment(statusLineSession, "GT_CREW")
		issue, _ = t.GetEnvironment(statusLineSession, "GT_ISSUE")
		role, _ = t.GetEnvironment(statusLineSession, "GT_ROLE")
	} else {
		// Fallback to process environment
		rigName = os.Getenv("GT_RIG")
		polecat = os.Getenv("GT_POLECAT")
		crew = os.Getenv("GT_CREW")
		issue = os.Getenv("GT_ISSUE")
		role = os.Getenv("GT_ROLE")
	}

	// Determine identity and output based on role
	if role == "mayor" || statusLineSession == "gt-mayor" {
		return runMayorStatusLine(t)
	}

	// Deacon status line
	if role == "deacon" || statusLineSession == "gt-deacon" {
		return runDeaconStatusLine(t)
	}

	// Witness status line (session naming: gt-<rig>-witness)
	if role == "witness" || strings.HasSuffix(statusLineSession, "-witness") {
		return runWitnessStatusLine(t, rigName)
	}

	// Refinery status line
	if role == "refinery" || strings.HasSuffix(statusLineSession, "-refinery") {
		return runRefineryStatusLine(rigName)
	}

	// Crew/Polecat status line
	return runWorkerStatusLine(t, statusLineSession, rigName, polecat, crew, issue)
}

// runWorkerStatusLine outputs status for crew or polecat sessions.
func runWorkerStatusLine(t *tmux.Tmux, session, rigName, polecat, crew, issue string) error {
	// Determine agent type and identity
	var icon, identity string
	if polecat != "" {
		icon = AgentTypeIcons[AgentPolecat]
		identity = fmt.Sprintf("%s/%s", rigName, polecat)
	} else if crew != "" {
		icon = AgentTypeIcons[AgentCrew]
		identity = fmt.Sprintf("%s/crew/%s", rigName, crew)
	}

	// Build status parts
	var parts []string

	// Try to get current work from beads if no issue env var
	currentWork := issue
	if currentWork == "" && session != "" {
		currentWork = getCurrentWork(t, session, 40)
	}

	// Add icon and current work
	if icon != "" {
		if currentWork != "" {
			parts = append(parts, fmt.Sprintf("%s %s", icon, currentWork))
		} else {
			parts = append(parts, icon)
		}
	} else if currentWork != "" {
		parts = append(parts, currentWork)
	}

	// Mail preview
	if identity != "" {
		unread, subject := getMailPreview(identity, 45)
		if unread > 0 {
			if subject != "" {
				parts = append(parts, fmt.Sprintf("\U0001F4EC %s", subject))
			} else {
				parts = append(parts, fmt.Sprintf("\U0001F4EC %d", unread))
			}
		}
	}

	// Output
	if len(parts) > 0 {
		fmt.Print(strings.Join(parts, " | ") + " |")
	}

	return nil
}

func runMayorStatusLine(t *tmux.Tmux) error {
	// Count active sessions by listing tmux sessions
	sessions, err := t.ListSessions()
	if err != nil {
		return nil // Silent fail
	}

	// Count polecats and rigs
	// Polecats: only actual polecats (not witnesses, refineries, deacon, crew)
	// Rigs: any rig with active sessions (witness, refinery, crew, or polecat)
	polecatCount := 0
	rigs := make(map[string]bool)
	for _, s := range sessions {
		agent := categorizeSession(s)
		if agent == nil {
			continue
		}
		// Count rigs from any rig-level agent (has non-empty Rig field)
		if agent.Rig != "" {
			rigs[agent.Rig] = true
		}
		// Count only polecats for polecat count
		if agent.Type == AgentPolecat {
			polecatCount++
		}
	}
	rigCount := len(rigs)

	// Get mayor mail with preview
	unread, subject := getMailPreview("mayor/", 45)

	// Build status
	var parts []string
	parts = append(parts, fmt.Sprintf("%d 😺", polecatCount))
	parts = append(parts, fmt.Sprintf("%d rigs", rigCount))
	if unread > 0 {
		if subject != "" {
			parts = append(parts, fmt.Sprintf("\U0001F4EC %s", subject))
		} else {
			parts = append(parts, fmt.Sprintf("\U0001F4EC %d", unread))
		}
	}

	fmt.Print(strings.Join(parts, " | ") + " |")
	return nil
}

// runDeaconStatusLine outputs status for the deacon session.
// Shows: active rigs, polecat count, mail preview
func runDeaconStatusLine(t *tmux.Tmux) error {
	// Count active rigs and polecats
	sessions, err := t.ListSessions()
	if err != nil {
		return nil // Silent fail
	}

	rigs := make(map[string]bool)
	polecatCount := 0
	for _, s := range sessions {
		agent := categorizeSession(s)
		if agent == nil {
			continue
		}
		if agent.Rig != "" {
			rigs[agent.Rig] = true
		}
		if agent.Type == AgentPolecat {
			polecatCount++
		}
	}
	rigCount := len(rigs)

	// Get deacon mail with preview
	unread, subject := getMailPreview("deacon/", 40)

	// Build status
	var parts []string
	parts = append(parts, fmt.Sprintf("%d rigs", rigCount))
	parts = append(parts, fmt.Sprintf("%d 😺", polecatCount))
	if unread > 0 {
		if subject != "" {
			parts = append(parts, fmt.Sprintf("\U0001F4EC %s", subject))
		} else {
			parts = append(parts, fmt.Sprintf("\U0001F4EC %d", unread))
		}
	}

	fmt.Print(strings.Join(parts, " | ") + " |")
	return nil
}

// runWitnessStatusLine outputs status for a witness session.
// Shows: polecat count, crew count, mail preview
func runWitnessStatusLine(t *tmux.Tmux, rigName string) error {
	if rigName == "" {
		// Try to extract from session name: gt-<rig>-witness
		if strings.HasSuffix(statusLineSession, "-witness") && strings.HasPrefix(statusLineSession, "gt-") {
			rigName = strings.TrimPrefix(strings.TrimSuffix(statusLineSession, "-witness"), "gt-")
		}
	}

	// Count polecats and crew in this rig
	sessions, err := t.ListSessions()
	if err != nil {
		return nil // Silent fail
	}

	polecatCount := 0
	crewCount := 0
	for _, s := range sessions {
		agent := categorizeSession(s)
		if agent == nil {
			continue
		}
		if agent.Rig == rigName {
			if agent.Type == AgentPolecat {
				polecatCount++
			} else if agent.Type == AgentCrew {
				crewCount++
			}
		}
	}

	// Get witness mail with preview
	identity := fmt.Sprintf("%s/witness", rigName)
	unread, subject := getMailPreview(identity, 35)

	// Build status
	var parts []string
	parts = append(parts, fmt.Sprintf("%d 😺", polecatCount))
	if crewCount > 0 {
		parts = append(parts, fmt.Sprintf("%d crew", crewCount))
	}
	if unread > 0 {
		if subject != "" {
			parts = append(parts, fmt.Sprintf("\U0001F4EC %s", subject))
		} else {
			parts = append(parts, fmt.Sprintf("\U0001F4EC %d", unread))
		}
	}

	fmt.Print(strings.Join(parts, " | ") + " |")
	return nil
}

// runRefineryStatusLine outputs status for a refinery session.
// Shows: MQ length, current item, mail preview
func runRefineryStatusLine(rigName string) error {
	if rigName == "" {
		// Try to extract from session name: gt-<rig>-refinery
		if strings.HasPrefix(statusLineSession, "gt-") && strings.HasSuffix(statusLineSession, "-refinery") {
			rigName = strings.TrimPrefix(statusLineSession, "gt-")
			rigName = strings.TrimSuffix(rigName, "-refinery")
		}
	}

	if rigName == "" {
		fmt.Printf("%s ? |", AgentTypeIcons[AgentRefinery])
		return nil
	}

	// Get refinery manager using shared helper
	mgr, _, _, err := getRefineryManager(rigName)
	if err != nil {
		// Fallback to simple status if we can't access refinery
		fmt.Printf("%s MQ: ? |", AgentTypeIcons[AgentRefinery])
		return nil
	}

	// Get queue
	queue, err := mgr.Queue()
	if err != nil {
		// Fallback to simple status if we can't read queue
		fmt.Printf("%s MQ: ? |", AgentTypeIcons[AgentRefinery])
		return nil
	}

	// Count pending items and find current item
	pending := 0
	var currentItem string
	for _, item := range queue {
		if item.Position == 0 && item.MR != nil {
			// Currently processing - show issue ID
			currentItem = item.MR.IssueID
		} else {
			pending++
		}
	}

	// Get refinery mail with preview
	identity := fmt.Sprintf("%s/refinery", rigName)
	unread, subject := getMailPreview(identity, 30)

	// Build status
	var parts []string
	if currentItem != "" {
		parts = append(parts, fmt.Sprintf("merging %s", currentItem))
		if pending > 0 {
			parts = append(parts, fmt.Sprintf("+%d queued", pending))
		}
	} else if pending > 0 {
		parts = append(parts, fmt.Sprintf("%d queued", pending))
	} else {
		parts = append(parts, "idle")
	}

	if unread > 0 {
		if subject != "" {
			parts = append(parts, fmt.Sprintf("\U0001F4EC %s", subject))
		} else {
			parts = append(parts, fmt.Sprintf("\U0001F4EC %d", unread))
		}
	}

	fmt.Print(strings.Join(parts, " | ") + " |")
	return nil
}

// getUnreadMailCount returns unread mail count for an identity.
// Fast path - returns 0 on any error.
func getUnreadMailCount(identity string) int {
	// Find workspace
	workDir, err := findMailWorkDir()
	if err != nil {
		return 0
	}

	// Create mailbox using beads
	mailbox := mail.NewMailboxBeads(identity, workDir)

	// Get count
	_, unread, err := mailbox.Count()
	if err != nil {
		return 0
	}

	return unread
}

// getMailPreview returns unread count and a truncated subject of the first unread message.
// Returns (count, subject) where subject is empty if no unread mail.
func getMailPreview(identity string, maxLen int) (int, string) {
	workDir, err := findMailWorkDir()
	if err != nil {
		return 0, ""
	}

	mailbox := mail.NewMailboxBeads(identity, workDir)

	// Get unread messages
	messages, err := mailbox.ListUnread()
	if err != nil || len(messages) == 0 {
		return 0, ""
	}

	// Get first message subject, truncated
	subject := messages[0].Subject
	if len(subject) > maxLen {
		subject = subject[:maxLen-1] + "…"
	}

	return len(messages), subject
}

// getCurrentWork returns a truncated title of the first in_progress issue.
// Uses the pane's working directory to find the beads.
func getCurrentWork(t *tmux.Tmux, session string, maxLen int) string {
	// Get the pane's working directory
	workDir, err := t.GetPaneWorkDir(session)
	if err != nil || workDir == "" {
		return ""
	}

	// Check if there's a .beads directory
	beadsDir := filepath.Join(workDir, ".beads")
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		return ""
	}

	// Query beads for in_progress issues
	b := beads.New(workDir)
	issues, err := b.List(beads.ListOptions{
		Status:   "in_progress",
		Priority: -1,
	})
	if err != nil || len(issues) == 0 {
		return ""
	}

	// Return first issue's ID and title, truncated
	issue := issues[0]
	display := fmt.Sprintf("%s: %s", issue.ID, issue.Title)
	if len(display) > maxLen {
		display = display[:maxLen-1] + "…"
	}
	return display
}
