package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

func runCrewAdd(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Parse rig/name format (e.g., "beads/emma" -> rig=beads, name=emma)
	// This prevents creating nested directories like crew/beads/emma
	rigName := crewRig
	if parsedRig, crewName, ok := parseRigSlashName(name); ok {
		if rigName == "" {
			rigName = parsedRig
		}
		name = crewName
	}

	// Find workspace
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Load rigs config
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	// Determine rig (if not already set from slash format or --rig flag)
	if rigName == "" {
		// Try to infer from cwd
		rigName, err = inferRigFromCwd(townRoot)
		if err != nil {
			return fmt.Errorf("could not determine rig (use --rig flag): %w", err)
		}
	}

	// Get rig
	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	r, err := rigMgr.GetRig(rigName)
	if err != nil {
		return fmt.Errorf("rig '%s' not found", rigName)
	}

	// Create crew manager
	crewGit := git.NewGit(r.Path)
	crewMgr := crew.NewManager(r, crewGit)

	// Create crew workspace
	fmt.Printf("Creating crew workspace %s in %s...\n", name, rigName)

	worker, err := crewMgr.Add(name, crewBranch)
	if err != nil {
		if err == crew.ErrCrewExists {
			return fmt.Errorf("crew workspace '%s' already exists", name)
		}
		return fmt.Errorf("creating crew workspace: %w", err)
	}

	fmt.Printf("%s Created crew workspace: %s/%s\n",
		style.Bold.Render("✓"), rigName, name)
	fmt.Printf("  Path: %s\n", worker.ClonePath)
	fmt.Printf("  Branch: %s\n", worker.Branch)
	fmt.Printf("  Mail: %s/mail/\n", worker.ClonePath)

	// Create agent bead for the crew worker
	rigBeadsPath := filepath.Join(r.Path, "mayor", "rig")
	bd := beads.New(rigBeadsPath)
	crewID := beads.CrewBeadID(rigName, name)
	if _, err := bd.Show(crewID); err != nil {
		// Agent bead doesn't exist, create it
		fields := &beads.AgentFields{
			RoleType:   "crew",
			Rig:        rigName,
			AgentState: "idle",
			RoleBead:   "gt-crew-role",
		}
		desc := fmt.Sprintf("Crew worker %s in %s - human-managed persistent workspace.", name, rigName)
		if _, err := bd.CreateAgentBead(crewID, desc, fields); err != nil {
			// Non-fatal: warn but don't fail the add
			style.PrintWarning("could not create agent bead: %v", err)
		} else {
			fmt.Printf("  Agent bead: %s\n", crewID)
		}
	}

	fmt.Printf("\n%s\n", style.Dim.Render("Start working with: cd "+worker.ClonePath))

	return nil
}
