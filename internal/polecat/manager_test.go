package polecat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

func TestStateIsActive(t *testing.T) {
	tests := []struct {
		state  State
		active bool
	}{
		{StateWorking, true},
		{StateDone, false},
		{StateStuck, false},
		// Legacy states are treated as active
		{StateIdle, true},
		{StateActive, true},
	}

	for _, tt := range tests {
		if got := tt.state.IsActive(); got != tt.active {
			t.Errorf("%s.IsActive() = %v, want %v", tt.state, got, tt.active)
		}
	}
}

func TestStateIsWorking(t *testing.T) {
	tests := []struct {
		state   State
		working bool
	}{
		{StateIdle, false},
		{StateActive, false},
		{StateWorking, true},
		{StateDone, false},
		{StateStuck, false},
	}

	for _, tt := range tests {
		if got := tt.state.IsWorking(); got != tt.working {
			t.Errorf("%s.IsWorking() = %v, want %v", tt.state, got, tt.working)
		}
	}
}

func TestPolecatSummary(t *testing.T) {
	p := &Polecat{
		Name:  "Toast",
		State: StateWorking,
		Issue: "gt-abc",
	}

	summary := p.Summary()
	if summary.Name != "Toast" {
		t.Errorf("Name = %q, want Toast", summary.Name)
	}
	if summary.State != StateWorking {
		t.Errorf("State = %v, want StateWorking", summary.State)
	}
	if summary.Issue != "gt-abc" {
		t.Errorf("Issue = %q, want gt-abc", summary.Issue)
	}
}

func TestListEmpty(t *testing.T) {
	root := t.TempDir()
	r := &rig.Rig{
		Name: "test-rig",
		Path: root,
	}
	m := NewManager(r, git.NewGit(root))

	polecats, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(polecats) != 0 {
		t.Errorf("polecats count = %d, want 0", len(polecats))
	}
}

func TestGetNotFound(t *testing.T) {
	root := t.TempDir()
	r := &rig.Rig{
		Name: "test-rig",
		Path: root,
	}
	m := NewManager(r, git.NewGit(root))

	_, err := m.Get("nonexistent")
	if err != ErrPolecatNotFound {
		t.Errorf("Get = %v, want ErrPolecatNotFound", err)
	}
}

func TestRemoveNotFound(t *testing.T) {
	root := t.TempDir()
	r := &rig.Rig{
		Name: "test-rig",
		Path: root,
	}
	m := NewManager(r, git.NewGit(root))

	err := m.Remove("nonexistent", false)
	if err != ErrPolecatNotFound {
		t.Errorf("Remove = %v, want ErrPolecatNotFound", err)
	}
}

func TestPolecatDir(t *testing.T) {
	r := &rig.Rig{
		Name: "test-rig",
		Path: "/home/user/ai/test-rig",
	}
	m := NewManager(r, git.NewGit(r.Path))

	dir := m.polecatDir("Toast")
	expected := "/home/user/ai/test-rig/polecats/Toast"
	if dir != expected {
		t.Errorf("polecatDir = %q, want %q", dir, expected)
	}
}

func TestAssigneeID(t *testing.T) {
	r := &rig.Rig{
		Name: "test-rig",
		Path: "/home/user/ai/test-rig",
	}
	m := NewManager(r, git.NewGit(r.Path))

	id := m.assigneeID("Toast")
	expected := "test-rig/Toast"
	if id != expected {
		t.Errorf("assigneeID = %q, want %q", id, expected)
	}
}

// Note: State persistence tests removed - state is now derived from beads assignee field.
// Integration tests should verify beads-based state management.

func TestGetReturnsIdleWithoutBeads(t *testing.T) {
	// When beads is not available, Get should return StateIdle
	root := t.TempDir()
	polecatDir := filepath.Join(root, "polecats", "Test")
	if err := os.MkdirAll(polecatDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Create mayor/rig directory for beads (but no actual beads)
	mayorRigDir := filepath.Join(root, "mayor", "rig")
	if err := os.MkdirAll(mayorRigDir, 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	r := &rig.Rig{
		Name: "test-rig",
		Path: root,
	}
	m := NewManager(r, git.NewGit(root))

	// Get should return polecat with StateIdle (no beads = no assignment)
	polecat, err := m.Get("Test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if polecat.Name != "Test" {
		t.Errorf("Name = %q, want Test", polecat.Name)
	}
	if polecat.State != StateIdle {
		t.Errorf("State = %v, want StateIdle (beads not available)", polecat.State)
	}
}

func TestListWithPolecats(t *testing.T) {
	root := t.TempDir()

	// Create some polecat directories (state is now derived from beads, not state files)
	for _, name := range []string{"Toast", "Cheedo"} {
		polecatDir := filepath.Join(root, "polecats", name)
		if err := os.MkdirAll(polecatDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	// Create mayor/rig for beads path
	mayorRig := filepath.Join(root, "mayor", "rig")
	if err := os.MkdirAll(mayorRig, 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	r := &rig.Rig{
		Name: "test-rig",
		Path: root,
	}
	m := NewManager(r, git.NewGit(root))

	polecats, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(polecats) != 2 {
		t.Errorf("polecats count = %d, want 2", len(polecats))
	}
}

// Note: TestSetState, TestAssignIssue, and TestClearIssue were removed.
// These operations now require a running beads instance and are tested
// via integration tests. The unit tests here focus on testing the basic
// polecat lifecycle operations that don't require beads.

func TestSetStateWithoutBeads(t *testing.T) {
	// SetState should not error when beads is not available
	root := t.TempDir()
	polecatDir := filepath.Join(root, "polecats", "Test")
	if err := os.MkdirAll(polecatDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Create mayor/rig for beads path
	mayorRig := filepath.Join(root, "mayor", "rig")
	if err := os.MkdirAll(mayorRig, 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	r := &rig.Rig{
		Name: "test-rig",
		Path: root,
	}
	m := NewManager(r, git.NewGit(root))

	// SetState should succeed (no-op when no issue assigned)
	err := m.SetState("Test", StateActive)
	if err != nil {
		t.Errorf("SetState: %v (expected no error when no beads/issue)", err)
	}
}

func TestClearIssueWithoutAssignment(t *testing.T) {
	// ClearIssue should not error when no issue is assigned
	root := t.TempDir()
	polecatDir := filepath.Join(root, "polecats", "Test")
	if err := os.MkdirAll(polecatDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Create mayor/rig for beads path
	mayorRig := filepath.Join(root, "mayor", "rig")
	if err := os.MkdirAll(mayorRig, 0755); err != nil {
		t.Fatalf("mkdir mayor/rig: %v", err)
	}

	r := &rig.Rig{
		Name: "test-rig",
		Path: root,
	}
	m := NewManager(r, git.NewGit(root))

	// ClearIssue should succeed even when no issue assigned
	err := m.ClearIssue("Test")
	if err != nil {
		t.Errorf("ClearIssue: %v (expected no error when no assignment)", err)
	}
}

// TestInstallCLAUDETemplate verifies the polecat CLAUDE.md template is installed
// from mayor/rig/templates/ (not rig root) with correct variable substitution.
// This is a regression test for gt-si6am.
func TestInstallCLAUDETemplate(t *testing.T) {
	root := t.TempDir()

	// Create polecat directory
	polecatDir := filepath.Join(root, "polecats", "testcat")
	if err := os.MkdirAll(polecatDir, 0755); err != nil {
		t.Fatalf("mkdir polecat: %v", err)
	}

	// Create template at mayor/rig/templates/ (the correct location)
	templateDir := filepath.Join(root, "mayor", "rig", "templates")
	if err := os.MkdirAll(templateDir, 0755); err != nil {
		t.Fatalf("mkdir templates: %v", err)
	}

	// Write a template with variables
	templateContent := `# Polecat Context

**YOU ARE IN: {{rig}}/polecats/{{name}}/** - This is YOUR worktree.

Your Role: POLECAT
Your rig: {{rig}}
Your name: {{name}}
`
	templatePath := filepath.Join(templateDir, "polecat-CLAUDE.md")
	if err := os.WriteFile(templatePath, []byte(templateContent), 0644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	// Also create a WRONG template at rig root (the old buggy location)
	// This should NOT be used
	wrongTemplateDir := filepath.Join(root, "templates")
	if err := os.MkdirAll(wrongTemplateDir, 0755); err != nil {
		t.Fatalf("mkdir wrong templates: %v", err)
	}
	wrongContent := `# Mayor Context - THIS IS WRONG`
	if err := os.WriteFile(filepath.Join(wrongTemplateDir, "polecat-CLAUDE.md"), []byte(wrongContent), 0644); err != nil {
		t.Fatalf("write wrong template: %v", err)
	}

	// Create manager and install template
	r := &rig.Rig{
		Name: "gastown",
		Path: root,
	}
	m := NewManager(r, git.NewGit(root))

	err := m.installCLAUDETemplate(polecatDir, "testcat")
	if err != nil {
		t.Fatalf("installCLAUDETemplate: %v", err)
	}

	// Read the installed CLAUDE.md
	installedPath := filepath.Join(polecatDir, "CLAUDE.md")
	content, err := os.ReadFile(installedPath)
	if err != nil {
		t.Fatalf("read installed CLAUDE.md: %v", err)
	}

	// Verify it's the polecat template (not mayor)
	if !strings.Contains(string(content), "Polecat Context") {
		t.Error("CLAUDE.md should contain 'Polecat Context'")
	}
	if strings.Contains(string(content), "Mayor Context") {
		t.Error("CLAUDE.md should NOT contain 'Mayor Context' (wrong template used)")
	}

	// Verify variables were substituted
	if strings.Contains(string(content), "{{rig}}") {
		t.Error("{{rig}} should be substituted")
	}
	if strings.Contains(string(content), "{{name}}") {
		t.Error("{{name}} should be substituted")
	}
	if !strings.Contains(string(content), "gastown/polecats/testcat/") {
		t.Error("CLAUDE.md should contain substituted path 'gastown/polecats/testcat/'")
	}
	if !strings.Contains(string(content), "Your rig: gastown") {
		t.Error("CLAUDE.md should contain 'Your rig: gastown'")
	}
	if !strings.Contains(string(content), "Your name: testcat") {
		t.Error("CLAUDE.md should contain 'Your name: testcat'")
	}
}

// TestInstallCLAUDETemplateNotAtRigRoot verifies that templates at rig root
// (the old buggy location) are NOT used.
func TestInstallCLAUDETemplateNotAtRigRoot(t *testing.T) {
	root := t.TempDir()

	// Create polecat directory
	polecatDir := filepath.Join(root, "polecats", "testcat")
	if err := os.MkdirAll(polecatDir, 0755); err != nil {
		t.Fatalf("mkdir polecat: %v", err)
	}

	// Only create template at rig root (wrong location)
	// Do NOT create at mayor/rig/templates/
	wrongTemplateDir := filepath.Join(root, "templates")
	if err := os.MkdirAll(wrongTemplateDir, 0755); err != nil {
		t.Fatalf("mkdir wrong templates: %v", err)
	}
	wrongContent := `# Mayor Context - THIS IS WRONG`
	if err := os.WriteFile(filepath.Join(wrongTemplateDir, "polecat-CLAUDE.md"), []byte(wrongContent), 0644); err != nil {
		t.Fatalf("write wrong template: %v", err)
	}

	// Create manager and try to install template
	r := &rig.Rig{
		Name: "gastown",
		Path: root,
	}
	m := NewManager(r, git.NewGit(root))

	// Should not error (missing template is OK) but should NOT install wrong one
	err := m.installCLAUDETemplate(polecatDir, "testcat")
	if err != nil {
		t.Fatalf("installCLAUDETemplate: %v", err)
	}

	// CLAUDE.md should NOT exist (template not found at correct location)
	installedPath := filepath.Join(polecatDir, "CLAUDE.md")
	if _, err := os.Stat(installedPath); err == nil {
		content, _ := os.ReadFile(installedPath)
		if strings.Contains(string(content), "Mayor Context") {
			t.Error("Template from rig root was incorrectly used - should use mayor/rig/templates/")
		}
	}
}
