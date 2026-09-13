// Package setup exposes pure bundle validation during the Go transition.
// File access and installation remain disabled until the protected installer exists.
package setup

import (
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/command"
)

// Bundle is the validated setup data shared with the operator command.
type Bundle = command.SetupBundle

// Parse validates already-read bytes without opening credentials or changing state.
func Parse(raw []byte, now time.Time) (Bundle, error) {
	return command.ParseSetupBundle(raw, "", now)
}
