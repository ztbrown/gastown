# Harness Documentation

A **harness** is the installation directory for Gas Town - the top-level workspace where your multi-agent system lives. The terms "harness" and "town" are used interchangeably.

## What is a Harness?

Think of a harness as:
- **Physical**: A directory on your filesystem (e.g., `~/gt/`)
- **Logical**: The container for all your rigs, agents, and coordination infrastructure
- **Operational**: The root from which the Mayor coordinates work across projects

A harness is NOT a git repository itself. It's a pure container that holds:
- Town-level configuration in `mayor/`
- Town-level beads database in `.beads/`
- Multiple rigs (each managing a project repository)

## Creating a Harness

```bash
gt install ~/gt              # Install Gas Town at ~/gt
gt install                   # Install at current directory
```

This creates the harness structure and initializes town-level beads with the `gm-` prefix.

## Directory Structure

```
~/gt/                              # Harness root (town)
├── CLAUDE.md                      # Mayor role prompting
├── .beads/                        # Town-level beads (prefix: gm-)
│   ├── beads.db                   # Mayor mail, coordination, handoffs
│   └── config.yaml
│
├── mayor/                         # Mayor's home directory
│   ├── town.json                  # Town configuration
│   ├── rigs.json                  # Registry of managed rigs
│   └── state.json                 # Mayor agent state
│
├── gastown/                       # A rig (project container)
├── wyvern/                        # Another rig
└── rigs/                          # Optional: managed rig clones
```

## Configuration Files

### mayor/town.json

The town configuration identifies this directory as a Gas Town harness:

```json
{
  "type": "town",
  "version": 1,
  "name": "stevey-gastown",
  "created_at": "2024-01-15T10:30:00Z"
}
```

The `type: "town"` field is the primary marker that identifies the harness root during workspace detection.

### mayor/rigs.json

Registry of all managed rigs:

```json
{
  "version": 1,
  "rigs": {
    "gastown": {
      "git_url": "https://github.com/steveyegge/gastown",
      "added_at": "2024-01-15T10:30:00Z"
    },
    "wyvern": {
      "git_url": "https://github.com/steveyegge/wyvern",
      "added_at": "2024-01-16T14:20:00Z"
    }
  }
}
```

### mayor/state.json

Mayor's agent state:

```json
{
  "role": "mayor",
  "last_active": "2025-12-19T10:00:00Z"
}
```

## Harness Detection

Gas Town finds the harness by walking up the directory tree looking for markers:

1. **Primary marker**: `mayor/town.json` - Definitive proof of harness root
2. **Secondary marker**: `mayor/` directory - Less definitive, continues searching upward

This two-tier search prevents false matches on rig-level `mayor/` directories.

```go
// Simplified detection logic
func FindHarness(startDir string) string {
    current := startDir
    for current != "/" {
        if exists(current + "/mayor/town.json") {
            return current  // Primary match
        }
        current = parent(current)
    }
    return ""
}
```

## Mayor: Town-Level vs Rig-Level

Gas Town has two levels of "mayor" presence:

### Town-Level Mayor (`<harness>/mayor/`)

- **Location**: At the harness root
- **Purpose**: Global coordination across all rigs
- **Configuration**: Contains `town.json`, `rigs.json`, `state.json`
- **Mailbox**: Uses town-level beads (`.beads/` with `gm-*` prefix)
- **Responsibility**: Work dispatch, cross-rig coordination, escalation handling

### Per-Rig Mayor (`<harness>/<rig>/mayor/rig/`)

- **Location**: Within each rig's directory
- **Purpose**: Provides the canonical git clone for the rig
- **Contents**: Full git clone of the project repository
- **Beads**: Holds the canonical `.beads/` directory for the rig
- **Special role**: Source of git worktrees for polecats

```
~/gt/                              # Harness root
├── mayor/                         # Town-level mayor home
│   ├── town.json                  # Town config (not project code)
│   ├── rigs.json
│   └── state.json
│
└── gastown/                       # Rig container
    └── mayor/                     # Per-rig mayor presence
        └── rig/                   # CANONICAL git clone
            ├── .git/
            ├── .beads/            # Canonical rig beads
            └── <project files>
```

## Beads Architecture

Gas Town uses a two-tier beads architecture:

| Level | Location | Prefix | Purpose |
|-------|----------|--------|---------|
| Town | `<harness>/.beads/` | `gm-*` | Mayor mail, cross-rig coordination, handoffs |
| Rig | `<rig>/mayor/rig/.beads/` | varies | Rig-local work tracking, agent communication |

### Beads Resolution

Rather than using a `.beads/redirect` file, Gas Town uses:

1. **Symlinks**: Rig root symlinks `.beads/` → `mayor/rig/.beads`
2. **Environment variable**: Agents receive `BEADS_DIR` pointing to the rig's beads

```bash
# When spawning agents
export BEADS_DIR=/path/to/rig/.beads
```

This ensures all agents in a rig share the same beads database, separate from any beads the project might have upstream.

## Relationship to Rigs

A harness contains multiple rigs. Each rig is:
- A container directory (NOT a git clone itself)
- Added via `gt rig add <name> <git-url>`
- Registered in `mayor/rigs.json`
- Has its own agents (witness, refinery, polecats)

```
Harness (town)
├── Rig 1 (gastown/)
│   ├── witness/
│   ├── refinery/
│   ├── polecats/
│   └── mayor/rig/  ← canonical clone
│
├── Rig 2 (wyvern/)
│   ├── witness/
│   ├── refinery/
│   ├── polecats/
│   └── mayor/rig/  ← canonical clone
│
└── Rig N...
```

## Naming Conventions

Recommended harness naming:
- Short, memorable name: `~/gt/` (gas town)
- Or project-specific: `~/workspace/`
- Or user-specific: `~/ai/`

Rig names typically match the project:
- `gastown` for the Gas Town project
- `wyvern` for the Wyvern project
- Use lowercase, no spaces

## Example: Complete Harness Setup

```bash
# 1. Install harness
gt install ~/gt

# 2. Add rigs
cd ~/gt
gt rig add gastown https://github.com/steveyegge/gastown
gt rig add wyvern https://github.com/steveyegge/wyvern

# 3. Verify structure
ls -la ~/gt/
# CLAUDE.md  .beads/  mayor/  gastown/  wyvern/

# 4. Check town config
cat ~/gt/mayor/town.json
# {"type":"town","version":1,"name":"..."}

# 5. Check registered rigs
cat ~/gt/mayor/rigs.json
# {"version":1,"rigs":{"gastown":{...},"wyvern":{...}}}
```

## See Also

- [Architecture Overview](architecture.md) - Full system architecture
- [Federation Design](federation-design.md) - Multi-machine deployment
- `gt doctor` - Health checks for harness and rigs
