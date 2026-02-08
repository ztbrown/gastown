//go:build integration

// Package cmd contains integration tests for beads db initialization after clone.
//
// Run with: go test -tags=integration ./internal/cmd -run TestBeadsDbInitAfterClone -v
//
// Bug: GitHub Issue #72
// When a repo with tracked .beads/ is added as a rig, beads.db doesn't exist
// (it's gitignored) and bd operations fail because no one runs `bd init`.
package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// extractJSON finds the first JSON object in output that may contain warning lines.
func extractJSON(output []byte) []byte {
	s := strings.TrimSpace(string(output))
	// Find the first '{' and last '}' to extract the JSON object
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return []byte(s[start : end+1])
	}
	return output
}

// createTrackedBeadsRepoWithIssues creates a git repo with .beads/ tracked that contains existing issues.
// This simulates a clone of a repo that has tracked beads with issues exported to issues.jsonl.
// The beads.db is NOT included (gitignored), so it must be reconstructed from exports.
func createTrackedBeadsRepoWithIssues(t *testing.T, path, prefix string, numIssues int) {
	t.Helper()

	// Create directory
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	// Initialize git repo with explicit main branch
	cmds := [][]string{
		{"git", "init", "--initial-branch=main"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test User"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Create initial file and commit (so we have something before beads)
	readmePath := filepath.Join(path, "README.md")
	if err := os.WriteFile(readmePath, []byte("# Test Repo\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	commitCmds := [][]string{
		{"git", "add", "."},
		{"git", "commit", "-m", "Initial commit"},
	}
	for _, args := range commitCmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Initialize beads
	beadsDir := filepath.Join(path, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	// Run bd init
	cmd := exec.Command("bd", "--no-daemon", "init", "--prefix", prefix)
	cmd.Dir = path
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init failed: %v\nOutput: %s", err, output)
	}

	// Create issues
	for i := 1; i <= numIssues; i++ {
		cmd = exec.Command("bd", "--no-daemon", "-q", "create",
			"--type", "task", "--title", fmt.Sprintf("Test issue %d", i))
		cmd.Dir = path
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bd create issue %d failed: %v\nOutput: %s", i, err, output)
		}
	}

	// Add .beads to git (simulating tracked beads)
	cmd = exec.Command("git", "add", ".beads")
	cmd.Dir = path
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add .beads: %v\n%s", err, out)
	}

	cmd = exec.Command("git", "commit", "-m", "Add beads with issues")
	cmd.Dir = path
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit beads: %v\n%s", err, out)
	}

	// Remove beads.db and WAL/SHM files to simulate what a clone would look like
	// (beads.db is gitignored, so cloned repos don't have it)
	for _, name := range []string{"beads.db", "beads.db-wal", "beads.db-shm"} {
		p := filepath.Join(beadsDir, name)
		os.Remove(p) // ignore error for WAL/SHM which may not exist
	}
}

// TestBeadsDbInitAfterClone tests that when a tracked beads repo is added as a rig,
// the beads database is properly initialized even though beads.db doesn't exist.
func TestBeadsDbInitAfterClone(t *testing.T) {
	// Skip if bd is not available
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not installed, skipping test")
	}

	tmpDir := t.TempDir()
	gtBinary := buildGT(t)

	t.Run("TrackedRepoWithExplicitPrefix", func(t *testing.T) {
		// GitHub Issue #72: gt rig add --adopt should register a rig with the given prefix
		// and bd operations should work afterwards.

		townRoot := filepath.Join(tmpDir, "town-prefix-test")

		// Install town first so the directory structure exists
		cmd := exec.Command(gtBinary, "install", townRoot, "--name", "prefix_test")
		cmd.Env = append(os.Environ(), "HOME="+tmpDir)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("gt install failed: %v\nOutput: %s", err, output)
		}

		// Create a repo with existing beads AND issues at <townRoot>/myrig
		existingRepo := filepath.Join(townRoot, "myrig")
		createTrackedBeadsRepoWithIssues(t, existingRepo, "existing_prefix", 3)

		// Add rig with --prefix to specify the beads prefix explicitly
		// Use --adopt and --force since this is a local directory without a remote
		cmd = exec.Command(gtBinary, "rig", "add", "myrig", existingRepo, "--adopt", "--force", "--prefix", "existing_prefix")
		cmd.Dir = townRoot
		cmd.Env = append(os.Environ(), "HOME="+tmpDir)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("gt rig add failed: %v\nOutput: %s", err, output)
		}

		// Verify routes.jsonl has the prefix
		routesContent, err := os.ReadFile(filepath.Join(townRoot, ".beads", "routes.jsonl"))
		if err != nil {
			t.Fatalf("read routes.jsonl: %v", err)
		}

		if !strings.Contains(string(routesContent), `"prefix":"existing_prefix-"`) {
			t.Errorf("routes.jsonl should contain existing_prefix, got:\n%s", routesContent)
		}

		// After adoption, beads.db doesn't exist (gitignored). Run bd init to reconstruct it.
		initCmd := exec.Command("bd", "--no-daemon", "init", "--prefix", "existing_prefix")
		initCmd.Dir = existingRepo
		if initOut, initErr := initCmd.CombinedOutput(); initErr != nil {
			t.Fatalf("bd init after adopt failed: %v\nOutput: %s", initErr, initOut)
		}

		// NOW TRY TO USE bd - verify the database was reconstructed
		cmd = exec.Command("bd", "--no-daemon", "--json", "-q", "create",
			"--type", "task", "--title", "test-from-rig")
		cmd.Dir = existingRepo
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("bd create failed after init: %v\nOutput: %s", err, output)
		}

		var result struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(extractJSON(output), &result); err != nil {
			t.Fatalf("parse output (%q): %v", string(output), err)
		}

		if !strings.HasPrefix(result.ID, "existing_prefix-") {
			t.Errorf("expected existing_prefix- prefix, got %s", result.ID)
		}
	})

	t.Run("TrackedRepoWithNoIssuesRequiresPrefix", func(t *testing.T) {
		// Regression test: When a tracked beads repo has NO issues (fresh init),
		// gt rig add must use the --prefix flag since there's nothing to detect from.

		townRoot := filepath.Join(tmpDir, "town-no-issues")

		// Install town first
		cmd := exec.Command(gtBinary, "install", townRoot, "--name", "no_issues_test")
		cmd.Env = append(os.Environ(), "HOME="+tmpDir)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("gt install failed: %v\nOutput: %s", err, output)
		}

		// Create a tracked beads repo with NO issues at <townRoot>/emptyrig
		emptyRepo := filepath.Join(townRoot, "emptyrig")
		createTrackedBeadsRepoWithNoIssues(t, emptyRepo, "empty_prefix")

		// Add rig WITH --prefix since there are no issues to detect from
		cmd = exec.Command(gtBinary, "rig", "add", "emptyrig", emptyRepo, "--adopt", "--force", "--prefix", "empty_prefix")
		cmd.Dir = townRoot
		cmd.Env = append(os.Environ(), "HOME="+tmpDir)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("gt rig add with --prefix failed: %v\nOutput: %s", err, output)
		}

		// Verify routes.jsonl has the prefix
		routesContent, err := os.ReadFile(filepath.Join(townRoot, ".beads", "routes.jsonl"))
		if err != nil {
			t.Fatalf("read routes.jsonl: %v", err)
		}

		if !strings.Contains(string(routesContent), `"prefix":"empty_prefix-"`) {
			t.Errorf("routes.jsonl should contain empty_prefix, got:\n%s", routesContent)
		}

		// After adoption, run bd init to reconstruct the database
		initCmd := exec.Command("bd", "--no-daemon", "init", "--prefix", "empty_prefix")
		initCmd.Dir = emptyRepo
		if initOut, initErr := initCmd.CombinedOutput(); initErr != nil {
			t.Fatalf("bd init after adopt failed: %v\nOutput: %s", initErr, initOut)
		}

		// Verify bd operations work with the configured prefix
		cmd = exec.Command("bd", "--no-daemon", "--json", "-q", "create",
			"--type", "task", "--title", "test-from-empty-repo")
		cmd.Dir = emptyRepo
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("bd create failed: %v\nOutput: %s", err, output)
		}

		var result struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(extractJSON(output), &result); err != nil {
			t.Fatalf("parse output (%q): %v", string(output), err)
		}

		if !strings.HasPrefix(result.ID, "empty_prefix-") {
			t.Errorf("expected empty_prefix- prefix, got %s", result.ID)
		}
	})

	t.Run("TrackedRepoWithPrefixOverride", func(t *testing.T) {
		// Test that --prefix on adopt sets the prefix for the rig registration
		// even when the repo already had beads initialized with a different prefix.

		townRoot := filepath.Join(tmpDir, "town-override")

		// Install town first
		cmd := exec.Command(gtBinary, "install", townRoot, "--name", "override_test")
		cmd.Env = append(os.Environ(), "HOME="+tmpDir)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("gt install failed: %v\nOutput: %s", err, output)
		}

		// Create a repo with beads prefix "original_prefix" at <townRoot>/overriderig
		overrideRepo := filepath.Join(townRoot, "overriderig")
		createTrackedBeadsRepoWithIssues(t, overrideRepo, "original_prefix", 2)

		// Add rig with a different --prefix - should use the provided prefix
		cmd = exec.Command(gtBinary, "rig", "add", "overriderig", overrideRepo, "--adopt", "--force", "--prefix", "custom_prefix")
		cmd.Dir = townRoot
		cmd.Env = append(os.Environ(), "HOME="+tmpDir)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gt rig add failed: %v\nOutput: %s", err, output)
		}

		// Verify routes.jsonl uses the provided prefix
		routesContent, err := os.ReadFile(filepath.Join(townRoot, ".beads", "routes.jsonl"))
		if err != nil {
			t.Fatalf("read routes.jsonl: %v", err)
		}

		if !strings.Contains(string(routesContent), `"prefix":"custom_prefix-"`) {
			t.Errorf("routes.jsonl should contain custom_prefix, got:\n%s", routesContent)
		}
	})

	t.Run("TrackedRepoWithDerivedPrefix", func(t *testing.T) {
		// Test the fallback behavior: when a tracked beads repo has NO issues
		// and NO --prefix is provided, gt rig add derives prefix from rig name.

		townRoot := filepath.Join(tmpDir, "town-derived")

		// Install town first
		cmd := exec.Command(gtBinary, "install", townRoot, "--name", "derived_test")
		cmd.Env = append(os.Environ(), "HOME="+tmpDir)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("gt install failed: %v\nOutput: %s", err, output)
		}

		// Create a tracked beads repo with NO issues at <townRoot>/testrig
		derivedRepo := filepath.Join(townRoot, "testrig")
		createTrackedBeadsRepoWithNoIssues(t, derivedRepo, "original_prefix")

		// Add rig WITHOUT --prefix - should derive from rig name "testrig"
		cmd = exec.Command(gtBinary, "rig", "add", "testrig", derivedRepo, "--adopt", "--force")
		cmd.Dir = townRoot
		cmd.Env = append(os.Environ(), "HOME="+tmpDir)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gt rig add (no --prefix) failed: %v\nOutput: %s", err, output)
		}

		// After adoption, run bd init to reconstruct the database
		initCmd := exec.Command("bd", "--no-daemon", "init")
		initCmd.Dir = derivedRepo
		if initOut, initErr := initCmd.CombinedOutput(); initErr != nil {
			t.Fatalf("bd init after adopt failed: %v\nOutput: %s", initErr, initOut)
		}

		// Verify bd operations work - the key test is that beads.db was initialized
		cmd = exec.Command("bd", "--no-daemon", "--json", "-q", "create",
			"--type", "task", "--title", "test-derived-prefix")
		cmd.Dir = derivedRepo
		output, err = cmd.Output()
		if err != nil {
			t.Fatalf("bd create failed (beads.db not initialized?): %v\nOutput: %s", err, output)
		}

		var result struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(extractJSON(output), &result); err != nil {
			t.Fatalf("parse output (%q): %v", string(output), err)
		}

		// The ID should have SOME prefix (derived from "testrig")
		if result.ID == "" {
			t.Error("expected non-empty issue ID")
		}
		t.Logf("Created issue with derived prefix: %s", result.ID)
	})
}

// createTrackedBeadsRepoWithNoIssues creates a git repo with .beads/ tracked but NO issues.
// This simulates a fresh bd init that was committed before any issues were created.
func createTrackedBeadsRepoWithNoIssues(t *testing.T, path, prefix string) {
	t.Helper()

	// Create directory
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	// Initialize git repo with explicit main branch
	cmds := [][]string{
		{"git", "init", "--initial-branch=main"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test User"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Create initial file and commit
	readmePath := filepath.Join(path, "README.md")
	if err := os.WriteFile(readmePath, []byte("# Test Repo\n"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}

	commitCmds := [][]string{
		{"git", "add", "."},
		{"git", "commit", "-m", "Initial commit"},
	}
	for _, args := range commitCmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = path
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Initialize beads
	beadsDir := filepath.Join(path, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	// Run bd init (creates beads.db but no issues)
	cmd := exec.Command("bd", "--no-daemon", "init", "--prefix", prefix)
	cmd.Dir = path
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init failed: %v\nOutput: %s", err, output)
	}

	// Add .beads to git (simulating tracked beads)
	cmd = exec.Command("git", "add", ".beads")
	cmd.Dir = path
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add .beads: %v\n%s", err, out)
	}

	cmd = exec.Command("git", "commit", "-m", "Add beads (no issues)")
	cmd.Dir = path
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit beads: %v\n%s", err, out)
	}

	// Remove beads.db and WAL/SHM files to simulate what a clone would look like
	for _, name := range []string{"beads.db", "beads.db-wal", "beads.db-shm"} {
		p := filepath.Join(beadsDir, name)
		os.Remove(p) // ignore error for WAL/SHM which may not exist
	}
}
