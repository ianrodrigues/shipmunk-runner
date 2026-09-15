package command

import (
	"context"
	"errors"
	"time"

	"github.com/ianrodrigues/shipmunk-runner/internal/profile"
	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
	"github.com/ianrodrigues/shipmunk-runner/internal/supervisor"
)

const codexCleanupTimeout = 90 * time.Second

// profileExecutor serializes one native profile across the provider lifecycle.
// The delegated executor owns transport resources; this wrapper releases the profile reservation only after transport cleanup.
type profileExecutor struct {
	store    *profile.Store
	executor supervisor.Executor
}

func newProfileExecutor(store *profile.Store, executor supervisor.Executor) (supervisor.Executor, error) {
	if store == nil || executor == nil {
		return nil, errors.New("Codex profile executor dependencies are incomplete")
	}
	return &profileExecutor{store: store, executor: executor}, nil
}

func (e *profileExecutor) Execute(ctx context.Context, claim protocol.Claim, input map[string]any, workspace string) (execution supervisor.Execution, resultErr error) {
	resultErr = e.store.WithExclusive(func(locked *profile.Store) error {
		if err := validateExecutionProfile(locked, claim); err != nil {
			return err
		}
		if err := locked.ReserveExecution(claim); err != nil {
			return err
		}

		execution, resultErr = e.executor.Execute(ctx, claim, input, workspace)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), codexCleanupTimeout)
		cleanupErr := e.executor.Cleanup(cleanupCtx, claim)
		cancel()
		if cleanupErr != nil {
			return errors.Join(resultErr, cleanupErr)
		}
		if err := locked.ReleaseExecution(claim); err != nil {
			return errors.Join(resultErr, err)
		}
		return resultErr
	})
	return execution, resultErr
}

func (e *profileExecutor) Cleanup(ctx context.Context, claim protocol.Claim) error {
	return e.store.WithExclusive(func(locked *profile.Store) error {
		matched, err := locked.AssertExecutionMatches(claim)
		if err != nil {
			return err
		}
		if !matched {
			return e.executor.Cleanup(ctx, claim)
		}
		if err := e.executor.Cleanup(ctx, claim); err != nil {
			return err
		}
		return locked.ReleaseExecution(claim)
	})
}

func (e *profileExecutor) Renew(claim protocol.Claim, expiry time.Time) error {
	if executor, ok := e.executor.(supervisor.ExecutorLease); ok {
		return executor.Renew(claim, expiry)
	}
	return nil
}

func (e *profileExecutor) close() error { return e.store.Close() }

func validateExecutionProfile(store *profile.Store, claim protocol.Claim) error {
	active, err := store.Read("active")
	if err != nil || active == nil {
		return errors.New("Codex profile is not active")
	}
	supervisorInput, _ := claim.Manifest["supervisor"].(map[string]any)
	credential, _ := supervisorInput["credential_reference"].(string)
	if active["profile_id"] != store.ProfileID() || claim.Manifest["profile_id"] != store.ProfileID() ||
		active["credential_reference"] != credential || credential == "" ||
		active["agent"] != "codex" || claim.Manifest["agent"] != "codex" ||
		active["auth_mode"] != "subscription" ||
		active["runtime_version"] != profile.CodexVersion || claim.Manifest["runtime_version"] != profile.CodexVersion {
		return errors.New("Codex profile binding does not match the claim")
	}
	if err := store.ValidateHome(); err != nil {
		return errors.New("Codex profile home is unsafe")
	}
	return nil
}
