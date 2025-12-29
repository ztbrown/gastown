package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// AgentBeadsCheck verifies that agent beads exist for all agents.
// This includes:
// - Global agents (deacon, mayor) - stored in first rig's beads
// - Per-rig agents (witness, refinery) - stored in each rig's beads
// - Crew workers - stored in each rig's beads
//
// Agent beads are created by gt rig add (see gt-h3hak, gt-pinkq) and gt crew add.
//
// NOTE: Currently, the beads library validates that agent IDs must start
// with 'gt-'. Rigs with different prefixes (like 'bd-') cannot have agent
// beads created until that validation is fixed in the beads repo.
type AgentBeadsCheck struct {
	FixableCheck
}

// NewAgentBeadsCheck creates a new agent beads check.
func NewAgentBeadsCheck() *AgentBeadsCheck {
	return &AgentBeadsCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "agent-beads-exist",
				CheckDescription: "Verify agent beads exist for all agents",
			},
		},
	}
}

// Run checks if agent beads exist for all expected agents.
func (c *AgentBeadsCheck) Run(ctx *CheckContext) *CheckResult {
	// Load routes to get prefixes (routes.jsonl is source of truth for prefixes)
	beadsDir := filepath.Join(ctx.TownRoot, ".beads")
	routes, err := beads.LoadRoutes(beadsDir)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Could not load routes.jsonl",
		}
	}

	// Build prefix -> rigName map from routes
	// Routes have format: prefix "gt-" -> path "gastown/mayor/rig"
	prefixToRig := make(map[string]string) // prefix (without hyphen) -> rigName
	for _, r := range routes {
		// Extract rig name from path like "gastown/mayor/rig"
		parts := strings.Split(r.Path, "/")
		if len(parts) >= 1 && parts[0] != "." {
			rigName := parts[0]
			prefix := strings.TrimSuffix(r.Prefix, "-")
			prefixToRig[prefix] = rigName
		}
	}

	if len(prefixToRig) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rigs with beads routes (agent beads created on rig add)",
		}
	}

	var missing []string
	var checked int

	// Find the first rig (by name, alphabetically) for global agents
	// Only consider gt-prefix rigs since other prefixes can't have agent beads yet
	var firstRigName string
	for prefix, rigName := range prefixToRig {
		if prefix != "gt" {
			continue // Skip non-gt prefixes for first rig selection
		}
		if firstRigName == "" || rigName < firstRigName {
			firstRigName = rigName
		}
	}

	// Check each rig for its agents
	var skipped []string
	for prefix, rigName := range prefixToRig {
		// Skip non-gt prefixes - beads library currently requires gt- prefix for agents
		// TODO: Remove this once beads validation is fixed to accept any prefix
		if prefix != "gt" {
			skipped = append(skipped, fmt.Sprintf("%s (%s-*)", rigName, prefix))
			continue
		}

		// Get beads client for this rig
		rigBeadsPath := filepath.Join(ctx.TownRoot, rigName, "mayor", "rig")
		bd := beads.New(rigBeadsPath)

		// Check rig-specific agents (using canonical naming: prefix-rig-role-name)
		witnessID := beads.WitnessBeadID(rigName)
		refineryID := beads.RefineryBeadID(rigName)

		if _, err := bd.Show(witnessID); err != nil {
			missing = append(missing, witnessID)
		}
		checked++

		if _, err := bd.Show(refineryID); err != nil {
			missing = append(missing, refineryID)
		}
		checked++

		// Check crew worker agents
		crewWorkers := listCrewWorkers(ctx.TownRoot, rigName)
		for _, workerName := range crewWorkers {
			crewID := beads.CrewBeadID(rigName, workerName)
			if _, err := bd.Show(crewID); err != nil {
				missing = append(missing, crewID)
			}
			checked++
		}

		// Check global agents in first rig
		if rigName == firstRigName {
			deaconID := beads.DeaconBeadID()
			mayorID := beads.MayorBeadID()

			if _, err := bd.Show(deaconID); err != nil {
				missing = append(missing, deaconID)
			}
			checked++

			if _, err := bd.Show(mayorID); err != nil {
				missing = append(missing, mayorID)
			}
			checked++
		}
	}

	if len(missing) == 0 {
		msg := fmt.Sprintf("All %d agent beads exist", checked)
		var details []string
		if len(skipped) > 0 {
			details = append(details, fmt.Sprintf("Skipped %d rig(s) with non-gt prefix (beads library limitation): %s",
				len(skipped), strings.Join(skipped, ", ")))
		}
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: msg,
			Details: details,
		}
	}

	details := missing
	if len(skipped) > 0 {
		details = append(details, fmt.Sprintf("Skipped %d rig(s) with non-gt prefix: %s",
			len(skipped), strings.Join(skipped, ", ")))
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: fmt.Sprintf("%d agent bead(s) missing", len(missing)),
		Details: details,
		FixHint: "Run 'gt doctor --fix' to create missing agent beads",
	}
}

// Fix creates missing agent beads.
func (c *AgentBeadsCheck) Fix(ctx *CheckContext) error {
	// Load routes to get prefixes (same as Run)
	beadsDir := filepath.Join(ctx.TownRoot, ".beads")
	routes, err := beads.LoadRoutes(beadsDir)
	if err != nil {
		return fmt.Errorf("loading routes.jsonl: %w", err)
	}

	// Build prefix -> rigName map from routes
	prefixToRig := make(map[string]string)
	for _, r := range routes {
		parts := strings.Split(r.Path, "/")
		if len(parts) >= 1 && parts[0] != "." {
			rigName := parts[0]
			prefix := strings.TrimSuffix(r.Prefix, "-")
			prefixToRig[prefix] = rigName
		}
	}

	if len(prefixToRig) == 0 {
		return nil // Nothing to fix
	}

	// Find the first rig for global agents (only gt-prefix rigs)
	var firstRigName string
	for prefix, rigName := range prefixToRig {
		if prefix != "gt" {
			continue
		}
		if firstRigName == "" || rigName < firstRigName {
			firstRigName = rigName
		}
	}

	// Create missing agents for each rig
	for prefix, rigName := range prefixToRig {
		// Skip non-gt prefixes - beads library currently requires gt- prefix for agents
		if prefix != "gt" {
			continue
		}

		rigBeadsPath := filepath.Join(ctx.TownRoot, rigName, "mayor", "rig")
		bd := beads.New(rigBeadsPath)

		// Create rig-specific agents if missing (using canonical naming: prefix-rig-role-name)
		witnessID := beads.WitnessBeadID(rigName)
		if _, err := bd.Show(witnessID); err != nil {
			fields := &beads.AgentFields{
				RoleType:   "witness",
				Rig:        rigName,
				AgentState: "idle",
				RoleBead:   "gt-witness-role",
			}
			desc := fmt.Sprintf("Witness for %s - monitors polecat health and progress.", rigName)
			if _, err := bd.CreateAgentBead(witnessID, desc, fields); err != nil {
				return fmt.Errorf("creating %s: %w", witnessID, err)
			}
		}

		refineryID := beads.RefineryBeadID(rigName)
		if _, err := bd.Show(refineryID); err != nil {
			fields := &beads.AgentFields{
				RoleType:   "refinery",
				Rig:        rigName,
				AgentState: "idle",
				RoleBead:   "gt-refinery-role",
			}
			desc := fmt.Sprintf("Refinery for %s - processes merge queue.", rigName)
			if _, err := bd.CreateAgentBead(refineryID, desc, fields); err != nil {
				return fmt.Errorf("creating %s: %w", refineryID, err)
			}
		}

		// Create crew worker agents if missing
		crewWorkers := listCrewWorkers(ctx.TownRoot, rigName)
		for _, workerName := range crewWorkers {
			crewID := beads.CrewBeadID(rigName, workerName)
			if _, err := bd.Show(crewID); err != nil {
				fields := &beads.AgentFields{
					RoleType:   "crew",
					Rig:        rigName,
					AgentState: "idle",
					RoleBead:   "gt-crew-role",
				}
				desc := fmt.Sprintf("Crew worker %s in %s - human-managed persistent workspace.", workerName, rigName)
				if _, err := bd.CreateAgentBead(crewID, desc, fields); err != nil {
					return fmt.Errorf("creating %s: %w", crewID, err)
				}
			}
		}

		// Create global agents in first rig if missing
		if rigName == firstRigName {
			deaconID := beads.DeaconBeadID()
			if _, err := bd.Show(deaconID); err != nil {
				fields := &beads.AgentFields{
					RoleType:   "deacon",
					Rig:        "",
					AgentState: "idle",
					RoleBead:   "gt-deacon-role",
				}
				desc := "Deacon (daemon beacon) - receives mechanical heartbeats, runs town plugins and monitoring."
				if _, err := bd.CreateAgentBead(deaconID, desc, fields); err != nil {
					return fmt.Errorf("creating %s: %w", deaconID, err)
				}
			}

			mayorID := beads.MayorBeadID()
			if _, err := bd.Show(mayorID); err != nil {
				fields := &beads.AgentFields{
					RoleType:   "mayor",
					Rig:        "",
					AgentState: "idle",
					RoleBead:   "gt-mayor-role",
				}
				desc := "Mayor - global coordinator, handles cross-rig communication and escalations."
				if _, err := bd.CreateAgentBead(mayorID, desc, fields); err != nil {
					return fmt.Errorf("creating %s: %w", mayorID, err)
				}
			}
		}
	}

	return nil
}

// listCrewWorkers returns the names of all crew workers in a rig.
func listCrewWorkers(townRoot, rigName string) []string {
	crewDir := filepath.Join(townRoot, rigName, "crew")
	entries, err := os.ReadDir(crewDir)
	if err != nil {
		return nil // No crew directory or can't read it
	}

	var workers []string
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			workers = append(workers, entry.Name())
		}
	}
	return workers
}
