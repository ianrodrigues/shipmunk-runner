package codex

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type versionDocker struct{ version string }

func (d versionDocker) Run(_ context.Context, _ time.Duration, _ int, _ io.Reader, args ...string) (transportResult, error) {
	if strings.Join(args, " ") != "version --format "+dockerVersionFormat {
		return transportResult{}, errors.New("unexpected Docker side effect")
	}
	return transportResult{stdout: []byte(d.version)}, nil
}

func TestReviewDockerRequirementsCheckClientAndLinuxEngine(t *testing.T) {
	for _, test := range []struct {
		name, version string
		valid         bool
	}{
		{"minimum", "26.0.0\n26.0.0\nlinux\n", true},
		{"newer", "29.1.0\n28.5.2\nlinux\n", true},
		{"vendor build", "28.5.2+dfsg1\n28.5.2-0ubuntu1\nlinux\n", true},
		{"old client", "25.0.5\n29.0.0\nlinux\n", false},
		{"old engine", "29.0.0\n25.0.5\nlinux\n", false},
		{"nonlinux", "29.0.0\n29.0.0\nwindows\n", false},
		{"unavailable engine", "29.0.0\n\n", false},
		{"malformed", "Docker version 29\n29.0.0\nlinux\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := requireDocker(context.Background(), versionDocker{test.version})
			if (err == nil) != test.valid {
				t.Fatalf("version=%q err=%v", test.version, err)
			}
			if err != nil && !errors.Is(err, ErrDockerRequirements) {
				t.Fatalf("requirement was not explicit: %v", err)
			}
		})
	}
}
