# Comptroller: Budget Controller for Gas Town

## Why It Exists

Gas Town runs multiple autonomous agents (polecats, witnesses, refineries, crew,
deacon, mayor) around the clock. Each agent session consumes API tokens that cost
real money. Without active monitoring, spend can silently balloon — a single
deacon patrol loop or a chatty witness crash-loop can burn hundreds of dollars
in a day.

The **Comptroller** is the financial feedback loop that was missing. It exists to:

1. **Make spend visible.** Before the comptroller, nobody tracked whether the
   week's budget was 50% gone or 95% gone until the bill arrived. The comptroller
   surfaces live numbers every patrol cycle.

2. **Apply graduated pressure.** Rather than a binary on/off switch, the
   comptroller uses four budget zones (green → yellow → orange → red) with
   progressively stronger actions — from gentle nudges to sling freezes.

3. **Keep agents honest about model choice.** Opus is 15x more expensive than
   haiku. The comptroller tracks the opus ratio and flags when too much work is
   running on expensive models that don't need them.

4. **Prevent runaway loops from draining the budget.** The 2026-02-20 witness
   crash-loop burned $592 in a single day on gastown-witness alone. A comptroller
   running concurrent patrols would have caught this within 30 minutes and
   escalated to freeze spawning.

## How It Works

The comptroller is a town-level agent (like the deacon and mayor) that runs
ephemeral patrol cycles on haiku:

```
┌─────────────┐     ccusage      ┌──────────────┐
│ comptroller │ ──────────────→  │  API billing  │
│  (haiku)    │ ←────────────── │    data       │
└──────┬──────┘   spend data     └──────────────┘
       │
       │ gt mail send
       ▼
┌──────────────┐  budget zone   ┌──────────────┐
│    mayor     │ ◄────────────  │  witnesses   │
│              │  directives →  │  polecats    │
└──────────────┘                └──────────────┘
```

Each patrol cycle:
1. Reads current state from `~/gt/comptroller/state.json`
2. Pulls spend data via `ccusage` (daily and per-session breakdowns)
3. Calculates week-to-date spend, daily burn rate, and projected weekly total
4. Determines the budget zone based on configurable thresholds
5. Issues directives (mail to mayor, witnesses) appropriate to the zone
6. Updates state and hands off

## Budget Zones

| Zone | Threshold | Actions |
|------|-----------|---------|
| GREEN | < 50% | No restrictions, log and continue |
| YELLOW | 50–75% | Warn mayor, nudge high-burn sessions to handoff |
| ORANGE | 75–90% | Restrict witnesses to 1 polecat/rig on sonnet, nudge 80k+ sessions |
| RED | > 90% | Request sling freeze, only P0 work proceeds |

## Configuration

Budget parameters live in `~/gt/comptroller/settings.json`:

- `weekly_budget_usd` — the weekly spending cap (default: $1200)
- `alert_thresholds` — zone boundaries as fractions of budget
- `model_policy` — recommended model per role
- `max_opus_ratio` — flag if opus exceeds this fraction of total spend
- `handoff_nudge_threshold_tokens` — nudge sessions above this token count
- `patrol_interval_minutes` — how often to run (default: 30)

## Design Decisions

**Why haiku?** The comptroller reads JSON output from `ccusage`, does arithmetic,
and sends mail. It doesn't need reasoning power. Running on haiku costs ~$0.02
per patrol vs ~$0.30 on opus — practicing what it preaches.

**Why ephemeral sessions?** Each patrol cycle is one session that reads state,
acts, writes state, and exits. This prevents context bloat and keeps costs
predictable. The deacon or a cron job restarts it on schedule.

**Why mail-based directives?** Gas Town's inter-agent communication already runs
on `gt mail`. Using the same channel means witnesses and polecats don't need
new integrations — they already check mail on session start.

**Why advisory, not enforcement?** The comptroller can nudge and recommend, but
only the mayor can truly freeze operations. This prevents a budget bot from
killing critical P0 work during an incident. The mayor makes the final call.

## Relationship to Other Agents

| Agent | Comptroller interaction |
|-------|----------------------|
| **Mayor** | Receives budget reports, approves/denies sling freezes |
| **Deacon** | Peer patrol agent — deacon handles health, comptroller handles money |
| **Witnesses** | Receive spawn policy directives (model, concurrency limits) |
| **Polecats** | Receive handoff nudges when burning too many tokens |
| **Refineries** | Not directly managed (low cost, short-lived merges) |
