package command

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadPublicSetupFileSymlink(t *testing.T) {
	tmpDir := t.TempDir()
	// Resolve to canonical path to avoid symlinks in the temp directory path
	canonicalTmpDir, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("failed to resolve temp directory: %v", err)
	}
	tmpDir = canonicalTmpDir

	// Create a regular file
	regularFile := filepath.Join(tmpDir, "regular.txt")
	if err := os.WriteFile(regularFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("failed to create regular file: %v", err)
	}

	// Create a symlink to the regular file
	symlinkPath := filepath.Join(tmpDir, "symlink.txt")
	if err := os.Symlink(regularFile, symlinkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	// Create a directory with a symlink component in the path
	symlinkDir := filepath.Join(tmpDir, "symlink-dir")
	if err := os.Symlink(tmpDir, symlinkDir); err != nil {
		t.Fatalf("failed to create symlink dir: %v", err)
	}
	fileInSymlinkDir := filepath.Join(symlinkDir, "file.txt")

	tests := []struct {
		name       string
		path       string
		wantErr    bool
		wantReason string
	}{
		{
			name:       "file path contains symlink component",
			path:       fileInSymlinkDir,
			wantErr:    true,
			wantReason: symlinkDir,
		},
		{
			name:       "file is a symlink",
			path:       symlinkPath,
			wantErr:    true,
			wantReason: symlinkPath,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readPublicSetupFile(tt.path, 1024)
			if !tt.wantErr && err != nil {
				t.Fatalf("readPublicSetupFile(%q) failed: %v", tt.path, err)
			}
			if tt.wantErr && err == nil {
				t.Fatalf("readPublicSetupFile(%q) expected error but got none", tt.path)
			}
			if tt.wantErr && tt.wantReason != "" {
				fileErr, ok := err.(*setupFileError)
				if !ok {
					t.Fatalf("expected setupFileError, got %T", err)
				}
				if fileErr.Reason() != tt.wantReason {
					t.Errorf("readPublicSetupFile(%q) reason = %q, want %q", tt.path, fileErr.Reason(), tt.wantReason)
				}
			}
		})
	}
}

func TestReadPublicSetupFileExceedsLimit(t *testing.T) {
	tmpDir := t.TempDir()
	// Resolve to canonical path to avoid symlinks in the temp directory path
	canonicalTmpDir, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("failed to resolve temp directory: %v", err)
	}
	tmpDir = canonicalTmpDir

	// Create a file that exceeds the limit
	largeFile := filepath.Join(tmpDir, "large.txt")
	data := make([]byte, 1025)
	if err := os.WriteFile(largeFile, data, 0o644); err != nil {
		t.Fatalf("failed to create large file: %v", err)
	}

	// Try to read with a smaller limit
	_, err = readPublicSetupFile(largeFile, 1024)
	if err == nil {
		t.Fatalf("readPublicSetupFile(%q) expected error but got none", largeFile)
	}

	fileErr, ok := err.(*setupFileError)
	if !ok {
		t.Fatalf("expected setupFileError, got %T", err)
	}
	if fileErr.Reason() != "exceeds its limit" {
		t.Errorf("readPublicSetupFile(%q) reason = %q, want %q", largeFile, fileErr.Reason(), "exceeds its limit")
	}
}

func TestReadPublicSetupFileHardLink(t *testing.T) {
	tmpDir := t.TempDir()
	// Resolve to canonical path to avoid symlinks in the temp directory path
	canonicalTmpDir, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("failed to resolve temp directory: %v", err)
	}
	tmpDir = canonicalTmpDir

	// Create a regular file
	originalFile := filepath.Join(tmpDir, "original.txt")
	if err := os.WriteFile(originalFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("failed to create original file: %v", err)
	}

	// Create a hard link to the file
	hardLinkPath := filepath.Join(tmpDir, "hardlink.txt")
	if err := os.Link(originalFile, hardLinkPath); err != nil {
		t.Fatalf("failed to create hard link: %v", err)
	}

	// Try to read the hard link
	_, err = readPublicSetupFile(hardLinkPath, 1024)
	if err == nil {
		t.Fatalf("readPublicSetupFile(%q) expected error but got none", hardLinkPath)
	}

	fileErr, ok := err.(*setupFileError)
	if !ok {
		t.Fatalf("expected setupFileError, got %T", err)
	}
	if fileErr.Reason() != "hard-linked" {
		t.Errorf("readPublicSetupFile(%q) reason = %q, want %q", hardLinkPath, fileErr.Reason(), "hard-linked")
	}
}
