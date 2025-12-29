package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var statusJSON bool

var statusCmd = &cobra.Command{
	Use:     "status",
	Aliases: []string{"stat"},
	GroupID: GroupDiag,
	Short:   "Show overall town status",
	Long: `Display the current status of the Gas Town workspace.

Shows town name, registered rigs, active polecats, and witness status.`,
	RunE: runStatus,
}

func init() {
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output as JSON")
	rootCmd.AddCommand(statusCmd)
}

// TownStatus represents the overall status of the workspace.
type TownStatus struct {
	Name     string        `json:"name"`
	Location string        `json:"location"`
	Agents   []AgentRuntime `json:"agents"`   // Global agents (Mayor, Deacon)
	Rigs     []RigStatus   `json:"rigs"`
	Summary  StatusSum     `json:"summary"`
}

// AgentRuntime represents the runtime state of an agent.
type AgentRuntime struct {
	Name         string `json:"name"`                    // Display name (e.g., "mayor", "witness")
	Address      string `json:"address"`                 // Full address (e.g., "gastown/witness")
	Session      string `json:"session"`                 // tmux session name
	Role         string `json:"role"`                    // Role type
	Running      bool   `json:"running"`                 // Is tmux session running?
	HasWork      bool   `json:"has_work"`                // Has pinned work?
	WorkTitle    string `json:"work_title,omitempty"`    // Title of pinned work
	HookBead     string `json:"hook_bead,omitempty"`     // Pinned bead ID from agent bead
	State        string `json:"state,omitempty"`         // Agent state from agent bead
	UnreadMail   int    `json:"unread_mail"`             // Number of unread messages
	FirstSubject string `json:"first_subject,omitempty"` // Subject of first unread message
}

// RigStatus represents status of a single rig.
type RigStatus struct {
	Name         string          `json:"name"`
	Polecats     []string        `json:"polecats"`
	PolecatCount int             `json:"polecat_count"`
	Crews        []string        `json:"crews"`
	CrewCount    int             `json:"crew_count"`
	HasWitness   bool            `json:"has_witness"`
	HasRefinery  bool            `json:"has_refinery"`
	Hooks        []AgentHookInfo `json:"hooks,omitempty"`
	Agents       []AgentRuntime  `json:"agents,omitempty"` // Runtime state of all agents in rig
}

// AgentHookInfo represents an agent's hook (pinned work) status.
type AgentHookInfo struct {
	Agent    string `json:"agent"`              // Agent address (e.g., "gastown/toast", "gastown/witness")
	Role     string `json:"role"`               // Role type (polecat, crew, witness, refinery)
	HasWork  bool   `json:"has_work"`           // Whether agent has pinned work
	Molecule string `json:"molecule,omitempty"` // Attached molecule ID
	Title    string `json:"title,omitempty"`    // Pinned bead title
}

// StatusSum provides summary counts.
type StatusSum struct {
	RigCount      int `json:"rig_count"`
	PolecatCount  int `json:"polecat_count"`
	CrewCount     int `json:"crew_count"`
	WitnessCount  int `json:"witness_count"`
	RefineryCount int `json:"refinery_count"`
	ActiveHooks   int `json:"active_hooks"`
}

func runStatus(cmd *cobra.Command, args []string) error {
	// Find town root
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Load town config
	townConfigPath := constants.MayorTownPath(townRoot)
	townConfig, err := config.LoadTownConfig(townConfigPath)
	if err != nil {
		// Try to continue without config
		townConfig = &config.TownConfig{Name: filepath.Base(townRoot)}
	}

	// Load rigs config
	rigsConfigPath := constants.MayorRigsPath(townRoot)
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		// Empty config if file doesn't exist
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	// Create rig manager
	g := git.NewGit(townRoot)
	mgr := rig.NewManager(townRoot, rigsConfig, g)

	// Create tmux instance for runtime checks
	t := tmux.NewTmux()

	// Discover rigs
	rigs, err := mgr.DiscoverRigs()
	if err != nil {
		return fmt.Errorf("discovering rigs: %w", err)
	}

	// Create beads instance for agent bead lookups (gastown rig holds gt- prefix beads)
	gastownBeadsPath := filepath.Join(townRoot, "gastown", "mayor", "rig")
	agentBeads := beads.New(gastownBeadsPath)

	// Create mail router for inbox lookups
	mailRouter := mail.NewRouter(townRoot)

	// Build status
	status := TownStatus{
		Name:     townConfig.Name,
		Location: townRoot,
		Agents:   discoverGlobalAgents(t, agentBeads, mailRouter),
		Rigs:     make([]RigStatus, 0, len(rigs)),
	}

	for _, r := range rigs {
		rs := RigStatus{
			Name:         r.Name,
			Polecats:     r.Polecats,
			PolecatCount: len(r.Polecats),
			HasWitness:   r.HasWitness,
			HasRefinery:  r.HasRefinery,
		}

		// Count crew workers
		crewGit := git.NewGit(r.Path)
		crewMgr := crew.NewManager(r, crewGit)
		if workers, err := crewMgr.List(); err == nil {
			for _, w := range workers {
				rs.Crews = append(rs.Crews, w.Name)
			}
			rs.CrewCount = len(workers)
		}

		// Discover hooks for all agents in this rig
		rs.Hooks = discoverRigHooks(r, rs.Crews)
		for _, hook := range rs.Hooks {
			if hook.HasWork {
				status.Summary.ActiveHooks++
			}
		}

		// Discover runtime state for all agents in this rig
		rs.Agents = discoverRigAgents(t, r, rs.Crews, agentBeads, mailRouter)

		status.Rigs = append(status.Rigs, rs)

		// Update summary
		status.Summary.PolecatCount += len(r.Polecats)
		status.Summary.CrewCount += rs.CrewCount
		if r.HasWitness {
			status.Summary.WitnessCount++
		}
		if r.HasRefinery {
			status.Summary.RefineryCount++
		}
	}
	status.Summary.RigCount = len(rigs)

	// Output
	if statusJSON {
		return outputStatusJSON(status)
	}
	return outputStatusText(status)
}

func outputStatusJSON(status TownStatus) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(status)
}

func outputStatusText(status TownStatus) error {
	// Header
	fmt.Printf("%s %s\n", style.Bold.Render("Town:"), status.Name)
	fmt.Printf("%s\n\n", style.Dim.Render(status.Location))

	// Role icons
	roleIcons := map[string]string{
		"mayor":       "🎩",
		"coordinator": "🎩",
		"deacon":      "🔔",
		"health-check": "🔔",
		"witness":     "👁",
		"refinery":    "🏭",
		"crew":        "👷",
		"polecat":     "😺",
	}

	// Global Agents (Mayor, Deacon)
	for _, agent := range status.Agents {
		icon := roleIcons[agent.Role]
		if icon == "" {
			icon = roleIcons[agent.Name]
		}
		fmt.Printf("%s %s\n", icon, style.Bold.Render(capitalizeFirst(agent.Name)))
		renderAgentDetails(agent, "   ", nil)
		fmt.Println()
	}

	if len(status.Rigs) == 0 {
		fmt.Printf("%s\n", style.Dim.Render("No rigs registered. Use 'gt rig add' to add one."))
		return nil
	}

	// Rigs
	for _, r := range status.Rigs {
		// Rig header with separator
		fmt.Printf("─── %s ───────────────────────────────────────────\n\n", style.Bold.Render(r.Name+"/"))

		// Group agents by role
		var witnesses, refineries, crews, polecats []AgentRuntime
		for _, agent := range r.Agents {
			switch agent.Role {
			case "witness":
				witnesses = append(witnesses, agent)
			case "refinery":
				refineries = append(refineries, agent)
			case "crew":
				crews = append(crews, agent)
			case "polecat":
				polecats = append(polecats, agent)
			}
		}

		// Witness
		if len(witnesses) > 0 {
			fmt.Printf("%s %s\n", roleIcons["witness"], style.Bold.Render("Witness"))
			for _, agent := range witnesses {
				renderAgentDetails(agent, "   ", r.Hooks)
			}
			fmt.Println()
		}

		// Refinery
		if len(refineries) > 0 {
			fmt.Printf("%s %s\n", roleIcons["refinery"], style.Bold.Render("Refinery"))
			for _, agent := range refineries {
				renderAgentDetails(agent, "   ", r.Hooks)
			}
			fmt.Println()
		}

		// Crew
		if len(crews) > 0 {
			fmt.Printf("%s %s (%d)\n", roleIcons["crew"], style.Bold.Render("Crew"), len(crews))
			for _, agent := range crews {
				renderAgentDetails(agent, "   ", r.Hooks)
			}
			fmt.Println()
		}

		// Polecats
		if len(polecats) > 0 {
			fmt.Printf("%s %s (%d)\n", roleIcons["polecat"], style.Bold.Render("Polecats"), len(polecats))
			for _, agent := range polecats {
				renderAgentDetails(agent, "   ", r.Hooks)
			}
			fmt.Println()
		}

		// No agents
		if len(witnesses) == 0 && len(refineries) == 0 && len(crews) == 0 && len(polecats) == 0 {
			fmt.Printf("   %s\n\n", style.Dim.Render("(no agents)"))
		}
	}

	return nil
}

// renderAgentDetails renders full agent bead details
func renderAgentDetails(agent AgentRuntime, indent string, hooks []AgentHookInfo) {
	// Line 1: Agent bead ID + status
	statusStr := style.Success.Render("running")
	if !agent.Running {
		statusStr = style.Error.Render("stopped")
	}

	stateInfo := ""
	if agent.State != "" && agent.State != "idle" && agent.State != "running" {
		stateInfo = style.Dim.Render(fmt.Sprintf(" [%s]", agent.State))
	}

	// Build agent bead ID using canonical naming: prefix-rig-role-name
	agentBeadID := "gt-" + agent.Name
	if agent.Address != "" && agent.Address != agent.Name {
		// Use address for full path agents like gastown/crew/joe → gt-gastown-crew-joe
		addr := strings.TrimSuffix(agent.Address, "/") // Remove trailing slash for global agents
		parts := strings.Split(addr, "/")
		if len(parts) == 1 {
			// Global agent: mayor/, deacon/ → gt-mayor, gt-deacon
			agentBeadID = beads.AgentBeadID("", parts[0], "")
		} else if len(parts) >= 2 {
			rig := parts[0]
			if parts[1] == "crew" && len(parts) >= 3 {
				agentBeadID = beads.CrewBeadID(rig, parts[2])
			} else if parts[1] == "witness" {
				agentBeadID = beads.WitnessBeadID(rig)
			} else if parts[1] == "refinery" {
				agentBeadID = beads.RefineryBeadID(rig)
			} else if len(parts) == 2 {
				// polecat: rig/name
				agentBeadID = beads.PolecatBeadID(rig, parts[1])
			}
		}
	}

	fmt.Printf("%s%s %s%s\n", indent, style.Dim.Render(agentBeadID), statusStr, stateInfo)

	// Line 2: Hook bead (pinned work)
	hookStr := style.Dim.Render("(none)")
	hookBead := agent.HookBead
	hookTitle := agent.WorkTitle

	// Fall back to hooks array if agent bead doesn't have hook info
	if hookBead == "" && hooks != nil {
		for _, h := range hooks {
			if h.Agent == agent.Address && h.HasWork {
				hookBead = h.Molecule
				hookTitle = h.Title
				break
			}
		}
	}

	if hookBead != "" {
		if hookTitle != "" {
			hookStr = fmt.Sprintf("%s → %s", hookBead, truncateWithEllipsis(hookTitle, 40))
		} else {
			hookStr = hookBead
		}
	} else if hookTitle != "" {
		// Has title but no molecule ID
		hookStr = truncateWithEllipsis(hookTitle, 50)
	}

	fmt.Printf("%s  hook: %s\n", indent, hookStr)

	// Line 3: Mail (if any unread)
	if agent.UnreadMail > 0 {
		mailStr := fmt.Sprintf("📬 %d unread", agent.UnreadMail)
		if agent.FirstSubject != "" {
			mailStr = fmt.Sprintf("📬 %d unread → %s", agent.UnreadMail, truncateWithEllipsis(agent.FirstSubject, 35))
		}
		fmt.Printf("%s  mail: %s\n", indent, mailStr)
	}
}

// formatHookInfo formats the hook bead and title for display
func formatHookInfo(hookBead, title string, maxLen int) string {
	if hookBead == "" {
		return ""
	}
	if title == "" {
		return fmt.Sprintf(" → %s", hookBead)
	}
	title = truncateWithEllipsis(title, maxLen)
	return fmt.Sprintf(" → %s", title)
}

// truncateWithEllipsis shortens a string to maxLen, adding "..." if truncated
func truncateWithEllipsis(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen < 4 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// capitalizeFirst capitalizes the first letter of a string
func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-32) + s[1:]
}

// discoverRigHooks finds all hook attachments for agents in a rig.
// It scans polecats, crew workers, witness, and refinery for handoff beads.
func discoverRigHooks(r *rig.Rig, crews []string) []AgentHookInfo {
	var hooks []AgentHookInfo

	// Create beads instance for the rig
	b := beads.New(r.Path)

	// Check polecats
	for _, name := range r.Polecats {
		hook := getAgentHook(b, name, r.Name+"/"+name, "polecat")
		hooks = append(hooks, hook)
	}

	// Check crew workers
	for _, name := range crews {
		hook := getAgentHook(b, name, r.Name+"/crew/"+name, "crew")
		hooks = append(hooks, hook)
	}

	// Check witness
	if r.HasWitness {
		hook := getAgentHook(b, "witness", r.Name+"/witness", "witness")
		hooks = append(hooks, hook)
	}

	// Check refinery
	if r.HasRefinery {
		hook := getAgentHook(b, "refinery", r.Name+"/refinery", "refinery")
		hooks = append(hooks, hook)
	}

	return hooks
}

// discoverGlobalAgents checks runtime state for town-level agents (Mayor, Deacon).
func discoverGlobalAgents(t *tmux.Tmux, agentBeads *beads.Beads, mailRouter *mail.Router) []AgentRuntime {
	var agents []AgentRuntime

	// Check Mayor
	mayorRunning, _ := t.HasSession(MayorSessionName)
	mayor := AgentRuntime{
		Name:    "mayor",
		Address: "mayor/",
		Session: MayorSessionName,
		Role:    "coordinator",
		Running: mayorRunning,
	}
	// Look up agent bead for hook/state
	if issue, fields, err := agentBeads.GetAgentBead("gt-mayor"); err == nil && issue != nil {
		mayor.HookBead = fields.HookBead
		mayor.State = fields.AgentState
		if fields.HookBead != "" {
			mayor.HasWork = true
			// Try to get the title of the pinned bead
			if pinnedIssue, err := agentBeads.Show(fields.HookBead); err == nil {
				mayor.WorkTitle = pinnedIssue.Title
			}
		}
	}
	// Get mail info
	populateMailInfo(&mayor, mailRouter)
	agents = append(agents, mayor)

	// Check Deacon
	deaconRunning, _ := t.HasSession(DeaconSessionName)
	deacon := AgentRuntime{
		Name:    "deacon",
		Address: "deacon/",
		Session: DeaconSessionName,
		Role:    "health-check",
		Running: deaconRunning,
	}
	// Look up agent bead for hook/state
	if issue, fields, err := agentBeads.GetAgentBead("gt-deacon"); err == nil && issue != nil {
		deacon.HookBead = fields.HookBead
		deacon.State = fields.AgentState
		if fields.HookBead != "" {
			deacon.HasWork = true
			if pinnedIssue, err := agentBeads.Show(fields.HookBead); err == nil {
				deacon.WorkTitle = pinnedIssue.Title
			}
		}
	}
	// Get mail info
	populateMailInfo(&deacon, mailRouter)
	agents = append(agents, deacon)

	return agents
}

// populateMailInfo fetches unread mail count and first subject for an agent
func populateMailInfo(agent *AgentRuntime, router *mail.Router) {
	if router == nil {
		return
	}
	mailbox, err := router.GetMailbox(agent.Address)
	if err != nil {
		return
	}
	_, unread, _ := mailbox.Count()
	agent.UnreadMail = unread
	if unread > 0 {
		if messages, err := mailbox.ListUnread(); err == nil && len(messages) > 0 {
			agent.FirstSubject = messages[0].Subject
		}
	}
}

// discoverRigAgents checks runtime state for all agents in a rig.
func discoverRigAgents(t *tmux.Tmux, r *rig.Rig, crews []string, agentBeads *beads.Beads, mailRouter *mail.Router) []AgentRuntime {
	var agents []AgentRuntime

	// Check Witness
	if r.HasWitness {
		sessionName := witnessSessionName(r.Name)
		running, _ := t.HasSession(sessionName)
		witness := AgentRuntime{
			Name:    "witness",
			Address: r.Name + "/witness",
			Session: sessionName,
			Role:    "witness",
			Running: running,
		}
		// Look up agent bead
		agentID := beads.WitnessBeadID(r.Name)
		if issue, fields, err := agentBeads.GetAgentBead(agentID); err == nil && issue != nil {
			witness.HookBead = fields.HookBead
			witness.State = fields.AgentState
			if fields.HookBead != "" {
				witness.HasWork = true
				if pinnedIssue, err := agentBeads.Show(fields.HookBead); err == nil {
					witness.WorkTitle = pinnedIssue.Title
				}
			}
		}
		populateMailInfo(&witness, mailRouter)
		agents = append(agents, witness)
	}

	// Check Refinery
	if r.HasRefinery {
		sessionName := fmt.Sprintf("gt-%s-refinery", r.Name)
		running, _ := t.HasSession(sessionName)
		refinery := AgentRuntime{
			Name:    "refinery",
			Address: r.Name + "/refinery",
			Session: sessionName,
			Role:    "refinery",
			Running: running,
		}
		// Look up agent bead
		agentID := beads.RefineryBeadID(r.Name)
		if issue, fields, err := agentBeads.GetAgentBead(agentID); err == nil && issue != nil {
			refinery.HookBead = fields.HookBead
			refinery.State = fields.AgentState
			if fields.HookBead != "" {
				refinery.HasWork = true
				if pinnedIssue, err := agentBeads.Show(fields.HookBead); err == nil {
					refinery.WorkTitle = pinnedIssue.Title
				}
			}
		}
		populateMailInfo(&refinery, mailRouter)
		agents = append(agents, refinery)
	}

	// Check Polecats
	for _, name := range r.Polecats {
		sessionName := fmt.Sprintf("gt-%s-%s", r.Name, name)
		running, _ := t.HasSession(sessionName)
		polecat := AgentRuntime{
			Name:    name,
			Address: r.Name + "/" + name,
			Session: sessionName,
			Role:    "polecat",
			Running: running,
		}
		// Look up agent bead
		agentID := beads.PolecatBeadID(r.Name, name)
		if issue, fields, err := agentBeads.GetAgentBead(agentID); err == nil && issue != nil {
			polecat.HookBead = fields.HookBead
			polecat.State = fields.AgentState
			if fields.HookBead != "" {
				polecat.HasWork = true
				if pinnedIssue, err := agentBeads.Show(fields.HookBead); err == nil {
					polecat.WorkTitle = pinnedIssue.Title
				}
			}
		}
		populateMailInfo(&polecat, mailRouter)
		agents = append(agents, polecat)
	}

	// Check Crew
	for _, name := range crews {
		sessionName := crewSessionName(r.Name, name)
		running, _ := t.HasSession(sessionName)
		crewAgent := AgentRuntime{
			Name:    name,
			Address: r.Name + "/crew/" + name,
			Session: sessionName,
			Role:    "crew",
			Running: running,
		}
		// Look up agent bead
		agentID := beads.CrewBeadID(r.Name, name)
		if issue, fields, err := agentBeads.GetAgentBead(agentID); err == nil && issue != nil {
			crewAgent.HookBead = fields.HookBead
			crewAgent.State = fields.AgentState
			if fields.HookBead != "" {
				crewAgent.HasWork = true
				if pinnedIssue, err := agentBeads.Show(fields.HookBead); err == nil {
					crewAgent.WorkTitle = pinnedIssue.Title
				}
			}
		}
		populateMailInfo(&crewAgent, mailRouter)
		agents = append(agents, crewAgent)
	}

	return agents
}

// getAgentHook retrieves hook status for a specific agent.
func getAgentHook(b *beads.Beads, role, agentAddress, roleType string) AgentHookInfo {
	hook := AgentHookInfo{
		Agent: agentAddress,
		Role:  roleType,
	}

	// Find handoff bead for this role
	handoff, err := b.FindHandoffBead(role)
	if err != nil || handoff == nil {
		return hook
	}

	// Check for attachment
	attachment := beads.ParseAttachmentFields(handoff)
	if attachment != nil && attachment.AttachedMolecule != "" {
		hook.HasWork = true
		hook.Molecule = attachment.AttachedMolecule
		hook.Title = handoff.Title
	} else if handoff.Description != "" {
		// Has content but no molecule - still has work
		hook.HasWork = true
		hook.Title = handoff.Title
	}

	return hook
}
