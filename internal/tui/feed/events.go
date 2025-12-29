package feed

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// EventSource represents a source of events
type EventSource interface {
	Events() <-chan Event
	Close() error
}

// BdActivitySource reads events from bd activity --follow
type BdActivitySource struct {
	cmd     *exec.Cmd
	events  chan Event
	cancel  context.CancelFunc
	workDir string
}

// NewBdActivitySource creates a new source that tails bd activity
func NewBdActivitySource(workDir string) (*BdActivitySource, error) {
	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, "bd", "activity", "--follow")
	cmd.Dir = workDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}

	source := &BdActivitySource{
		cmd:     cmd,
		events:  make(chan Event, 100),
		cancel:  cancel,
		workDir: workDir,
	}

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if event := parseBdActivityLine(line); event != nil {
				select {
				case source.events <- *event:
				default:
					// Drop event if channel full
				}
			}
		}
		close(source.events)
	}()

	return source, nil
}

// Events returns the event channel
func (s *BdActivitySource) Events() <-chan Event {
	return s.events
}

// Close stops the source
func (s *BdActivitySource) Close() error {
	s.cancel()
	return s.cmd.Wait()
}

// bd activity line pattern: [HH:MM:SS] SYMBOL BEAD_ID action · description
var bdActivityPattern = regexp.MustCompile(`^\[(\d{2}:\d{2}:\d{2})\]\s+([+→✓✗⊘📌])\s+(\S+)?\s*(\w+)?\s*·?\s*(.*)$`)

// parseBdActivityLine parses a line from bd activity output
func parseBdActivityLine(line string) *Event {
	matches := bdActivityPattern.FindStringSubmatch(line)
	if matches == nil {
		// Try simpler pattern
		return parseSimpleLine(line)
	}

	timeStr := matches[1]
	symbol := matches[2]
	beadID := matches[3]
	action := matches[4]
	message := matches[5]

	// Parse time (assume today)
	now := time.Now()
	t, err := time.Parse("15:04:05", timeStr)
	if err != nil {
		t = now
	} else {
		t = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, now.Location())
	}

	// Map symbol to event type
	eventType := "update"
	switch symbol {
	case "+":
		eventType = "create"
	case "→":
		eventType = "update"
	case "✓":
		eventType = "complete"
	case "✗":
		eventType = "fail"
	case "⊘":
		eventType = "delete"
	case "📌":
		eventType = "pin"
	}

	// Try to extract actor and rig from bead ID
	actor, rig, role := parseBeadContext(beadID)

	return &Event{
		Time:    t,
		Type:    eventType,
		Actor:   actor,
		Target:  beadID,
		Message: strings.TrimSpace(action + " " + message),
		Rig:     rig,
		Role:    role,
		Raw:     line,
	}
}

// parseSimpleLine handles lines that don't match the full pattern
func parseSimpleLine(line string) *Event {
	if strings.TrimSpace(line) == "" {
		return nil
	}

	// Try to extract timestamp
	var t time.Time
	if len(line) > 10 && line[0] == '[' {
		if idx := strings.Index(line, "]"); idx > 0 {
			timeStr := line[1:idx]
			now := time.Now()
			if parsed, err := time.Parse("15:04:05", timeStr); err == nil {
				t = time.Date(now.Year(), now.Month(), now.Day(),
					parsed.Hour(), parsed.Minute(), parsed.Second(), 0, now.Location())
			}
		}
	}

	if t.IsZero() {
		t = time.Now()
	}

	return &Event{
		Time:    t,
		Type:    "update",
		Message: line,
		Raw:     line,
	}
}

// parseBeadContext extracts actor/rig/role from a bead ID
// Uses canonical naming: prefix-rig-role-name
// Examples: gt-gastown-crew-joe, gt-gastown-witness, gt-mayor
func parseBeadContext(beadID string) (actor, rig, role string) {
	if beadID == "" {
		return
	}

	// Use the canonical parser
	parsedRig, parsedRole, name, ok := beads.ParseAgentBeadID(beadID)
	if !ok {
		return
	}

	rig = parsedRig
	role = parsedRole

	// Build actor identifier
	switch parsedRole {
	case "mayor", "deacon":
		actor = parsedRole
	case "witness", "refinery":
		actor = parsedRole
	case "crew":
		if name != "" {
			actor = parsedRig + "/crew/" + name
		} else {
			actor = parsedRole
		}
	case "polecat":
		if name != "" {
			actor = parsedRig + "/" + name
		} else {
			actor = parsedRole
		}
	}

	return
}

// JSONLSource reads events from a JSONL file (like .events.jsonl)
type JSONLSource struct {
	file    *os.File
	events  chan Event
	cancel  context.CancelFunc
}

// JSONLEvent is the structure of events in .events.jsonl
type JSONLEvent struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Actor     string `json:"actor"`
	Target    string `json:"target"`
	Message   string `json:"message"`
	Rig       string `json:"rig"`
	Role      string `json:"role"`
}

// NewJSONLSource creates a source that tails a JSONL file
func NewJSONLSource(filePath string) (*JSONLSource, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	source := &JSONLSource{
		file:   file,
		events: make(chan Event, 100),
		cancel: cancel,
	}

	go source.tail(ctx)

	return source, nil
}

// tail follows the file and sends events
func (s *JSONLSource) tail(ctx context.Context) {
	defer close(s.events)

	// Seek to end for live tailing
	s.file.Seek(0, 2)

	scanner := bufio.NewScanner(s.file)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for scanner.Scan() {
				line := scanner.Text()
				if event := parseJSONLLine(line); event != nil {
					select {
					case s.events <- *event:
					default:
					}
				}
			}
		}
	}
}

// Events returns the event channel
func (s *JSONLSource) Events() <-chan Event {
	return s.events
}

// Close stops the source
func (s *JSONLSource) Close() error {
	s.cancel()
	return s.file.Close()
}

// parseJSONLLine parses a JSONL event line
func parseJSONLLine(line string) *Event {
	if strings.TrimSpace(line) == "" {
		return nil
	}

	var je JSONLEvent
	if err := json.Unmarshal([]byte(line), &je); err != nil {
		return nil
	}

	t, err := time.Parse(time.RFC3339, je.Timestamp)
	if err != nil {
		t = time.Now()
	}

	return &Event{
		Time:    t,
		Type:    je.Type,
		Actor:   je.Actor,
		Target:  je.Target,
		Message: je.Message,
		Rig:     je.Rig,
		Role:    je.Role,
		Raw:     line,
	}
}

// FindBeadsDir finds the beads directory for the given working directory
func FindBeadsDir(workDir string) (string, error) {
	// Walk up looking for .beads
	dir := workDir
	for {
		beadsPath := filepath.Join(dir, ".beads")
		if info, err := os.Stat(beadsPath); err == nil && info.IsDir() {
			return beadsPath, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "", os.ErrNotExist
}
