package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/witness"
)

// Witness command flags
var (
	witnessStatusJSON bool
)

var witnessCmd = &cobra.Command{
	Use:     "witness",
	GroupID: GroupAgents,
	Short:   "View Witness status (per-rig patrol managed by Go daemon)",
	RunE:    requireSubcommand,
	Long: `View Witness status - the per-rig polecat health monitor.

The Witness role is now handled directly by the Go daemon, which:
  - Processes witness inboxes on each heartbeat tick
  - Detects stalled polecats (crashed or stuck mid-work)
  - Handles crash recovery and orphan cleanup

One Witness per rig. Role shortcuts: "witness" in mail/nudge addresses
resolves to this rig's Witness.`,
}

var witnessStatusCmd = &cobra.Command{
	Use:   "status <rig>",
	Short: "Show witness status",
	Long: `Show the status of a rig's Witness.

Displays running state and monitored polecats.`,
	Args: cobra.ExactArgs(1),
	RunE: runWitnessStatus,
}

func init() {
	// Status flags
	witnessStatusCmd.Flags().BoolVar(&witnessStatusJSON, "json", false, "Output as JSON")

	// Add subcommands
	witnessCmd.AddCommand(witnessStatusCmd)

	rootCmd.AddCommand(witnessCmd)
}

// WitnessStatusOutput is the JSON output format for witness status.
type WitnessStatusOutput struct {
	Running           bool     `json:"running"`
	RigName           string   `json:"rig_name"`
	Session           string   `json:"session,omitempty"`
	MonitoredPolecats []string `json:"monitored_polecats,omitempty"`
}

func runWitnessStatus(cmd *cobra.Command, args []string) error {
	rigName := args[0]

	// Get rig for polecat info
	_, r, err := getRig(rigName)
	if err != nil {
		return err
	}

	mgr := witness.NewManager(r)

	// ZFC: tmux is source of truth for running state
	running, _ := mgr.IsRunning()
	sessionInfo, _ := mgr.Status() // may be nil if not running

	// Polecats come from rig config, not state file
	polecats := r.Polecats

	// JSON output
	if witnessStatusJSON {
		output := WitnessStatusOutput{
			Running:           running,
			RigName:           rigName,
			MonitoredPolecats: polecats,
		}
		if sessionInfo != nil {
			output.Session = sessionInfo.Name
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(output)
	}

	// Human-readable output
	fmt.Printf("%s Witness: %s\n\n", style.Bold.Render(AgentTypeIcons[AgentWitness]), rigName)
	fmt.Printf("  Managed by: %s\n", style.Dim.Render("Go daemon"))

	if running {
		fmt.Printf("  Session: %s\n", style.Bold.Render("● running (legacy)"))
		if sessionInfo != nil {
			fmt.Printf("  Session name: %s\n", sessionInfo.Name)
		}
	} else {
		fmt.Printf("  Session: %s\n", style.Dim.Render("○ none"))
	}

	// Show monitored polecats
	fmt.Printf("\n  %s\n", style.Bold.Render("Monitored Polecats:"))
	if len(polecats) == 0 {
		fmt.Printf("    %s\n", style.Dim.Render("(none)"))
	} else {
		for _, p := range polecats {
			fmt.Printf("    • %s\n", p)
		}
	}

	return nil
}
