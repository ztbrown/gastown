package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyWorktreeExists(t *testing.T) {
	t.Run("valid worktree with .git file", func(t *testing.T) {
		dir := t.TempDir()
		// Write a .git file (as in real git worktrees)
		gitFile := filepath.Join(dir, ".git")
		if err := os.WriteFile(gitFile, []byte("gitdir: ../main/.git/worktrees/foo\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := verifyWorktreeExists(dir); err != nil {
			t.Errorf("expected no error for valid worktree, got: %v", err)
		}
	})

	t.Run("valid worktree with .git directory", func(t *testing.T) {
		dir := t.TempDir()
		// .git can also be a directory (full clone)
		gitDir := filepath.Join(dir, ".git")
		if err := os.MkdirAll(gitDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := verifyWorktreeExists(dir); err != nil {
			t.Errorf("expected no error for .git directory, got: %v", err)
		}
	})

	t.Run("directory does not exist", func(t *testing.T) {
		err := verifyWorktreeExists("/nonexistent/path/that/does/not/exist")
		if err == nil {
			t.Error("expected error for nonexistent directory, got nil")
		}
	})

	t.Run("directory exists but missing .git", func(t *testing.T) {
		dir := t.TempDir()
		err := verifyWorktreeExists(dir)
		if err == nil {
			t.Error("expected error for directory missing .git, got nil")
		}
	})

	t.Run("path is a file not a directory", func(t *testing.T) {
		dir := t.TempDir()
		filePath := filepath.Join(dir, "notadir")
		if err := os.WriteFile(filePath, []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
		err := verifyWorktreeExists(filePath)
		if err == nil {
			t.Error("expected error when path is a file, got nil")
		}
	})
}
