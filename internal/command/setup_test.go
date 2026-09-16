package command

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// t.TempDir() can sit under a symlink (/var on macOS), which this guard rejects.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	canonical, err := filepath.EvalSymlinks(tmpDir)
	if err != nil {
		t.Fatalf("failed to resolve temp directory: %v", err)
	}
	return canonical
}

func TestReadPublicSetupFileSymlink(t *testing.T) {
	tmpDir := canonicalTempDir(t)

	regularFile := filepath.Join(tmpDir, "regular.txt")
	if err := os.WriteFile(regularFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("failed to create regular file: %v", err)
	}

	symlinkPath := filepath.Join(tmpDir, "symlink.txt")
	if err := os.Symlink(regularFile, symlinkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	symlinkDir := filepath.Join(tmpDir, "symlink-dir")
	if err := os.Symlink(tmpDir, symlinkDir); err != nil {
		t.Fatalf("failed to create symlink dir: %v", err)
	}
	fileInSymlinkDir := filepath.Join(symlinkDir, "file.txt")

	missingComponentDir := filepath.Join(tmpDir, "does-not-exist")
	missingFile := filepath.Join(missingComponentDir, "missing.txt")

	tests := []struct {
		name       string
		path       string
		wantReason string
	}{
		{
			name:       "path has a symlink directory component",
			path:       fileInSymlinkDir,
			wantReason: "path component " + symlinkDir + " is a symlink",
		},
		{
			name:       "final path component is a symlink",
			path:       symlinkPath,
			wantReason: "path component " + symlinkPath + " is a symlink",
		},
		{
			name:       "path component does not exist",
			path:       missingFile,
			wantReason: "path component " + missingComponentDir + " is missing or unreadable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readPublicSetupFile(tt.path, 1024)
			if !errors.Is(err, errUnsafeSetupFile) {
				t.Fatalf("readPublicSetupFile(%q) error = %v, want errUnsafeSetupFile", tt.path, err)
			}
			if err.Error() != tt.wantReason {
				t.Errorf("readPublicSetupFile(%q) reason = %q, want %q", tt.path, err.Error(), tt.wantReason)
			}
		})
	}
}

func TestReadPublicSetupFileExceedsLimit(t *testing.T) {
	tmpDir := canonicalTempDir(t)

	largeFile := filepath.Join(tmpDir, "large.txt")
	if err := os.WriteFile(largeFile, make([]byte, 1025), 0o644); err != nil {
		t.Fatalf("failed to create large file: %v", err)
	}

	_, err := readPublicSetupFile(largeFile, 1024)
	if !errors.Is(err, errUnsafeSetupFile) {
		t.Fatalf("readPublicSetupFile(%q) error = %v, want errUnsafeSetupFile", largeFile, err)
	}
	if want := "exceeds its limit"; err.Error() != want {
		t.Errorf("readPublicSetupFile(%q) reason = %q, want %q", largeFile, err.Error(), want)
	}
}

func TestReadPublicSetupFileHardLink(t *testing.T) {
	tmpDir := canonicalTempDir(t)

	originalFile := filepath.Join(tmpDir, "original.txt")
	if err := os.WriteFile(originalFile, []byte("test"), 0o644); err != nil {
		t.Fatalf("failed to create original file: %v", err)
	}

	hardLinkPath := filepath.Join(tmpDir, "hardlink.txt")
	if err := os.Link(originalFile, hardLinkPath); err != nil {
		t.Fatalf("failed to create hard link: %v", err)
	}

	_, err := readPublicSetupFile(hardLinkPath, 1024)
	if !errors.Is(err, errUnsafeSetupFile) {
		t.Fatalf("readPublicSetupFile(%q) error = %v, want errUnsafeSetupFile", hardLinkPath, err)
	}
	if want := "is hard-linked"; err.Error() != want {
		t.Errorf("readPublicSetupFile(%q) reason = %q, want %q", hardLinkPath, err.Error(), want)
	}
}

func TestSetupFileMessage(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		err    error
		want   string
	}{
		{
			name:   "guard error appends the reason",
			prefix: "Runner release manifest is unsafe or invalid.",
			err:    fmt.Errorf("path component /var/x is a symlink%w", errUnsafeSetupFile),
			want:   "Runner release manifest is unsafe or invalid. path component /var/x is a symlink.",
		},
		{
			name:   "archive prefix appends the reason",
			prefix: "Runner release archive is unsafe or invalid.",
			err:    fmt.Errorf("exceeds its limit%w", errUnsafeSetupFile),
			want:   "Runner release archive is unsafe or invalid. exceeds its limit.",
		},
		{
			name:   "non-guard error leaves the prefix alone",
			prefix: "Runner release manifest is unsafe or invalid.",
			err:    errors.New("boom"),
			want:   "Runner release manifest is unsafe or invalid.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := setupFileMessage(tt.prefix, tt.err); got != tt.want {
				t.Errorf("setupFileMessage(%q, %v) = %q, want %q", tt.prefix, tt.err, got, tt.want)
			}
		})
	}
}
