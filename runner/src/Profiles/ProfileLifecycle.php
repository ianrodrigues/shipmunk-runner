<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Profiles;

use Closure;
use DateTimeImmutable;
use RuntimeException;
use Shipmunk\Runner\ControlPlaneException;
use Shipmunk\Runner\HostWatchdog;
use Shipmunk\Runner\Watchdog;
use Shipmunk\Runner\WatchdogLease;
use Throwable;

final readonly class ProfileLifecycle
{
    public function __construct(
        private ProfileControlPlane $controlPlane,
        private ProfileRuntime $runtime,
        private Watchdog $watchdog = new HostWatchdog,
        private ?Closure $diagnostic = null,
    ) {}

    /**
     * @return array{health: string, reason: ?string}
     */
    public function operate(
        ProfileStore $store,
        string $profile,
        string $operation,
        string $operationId,
    ): array {
        ProfileStore::identifier($profile);
        if ($store->profileId !== $profile) {
            throw new RuntimeException('Profile store identity mismatch.');
        }
        ProfileStore::identifier($operationId);
        if (! in_array($operation, ['login', 'probe', 'disconnect'], true)) {
            throw new RuntimeException('Unsupported profile operation.');
        }

        return $store->exclusively(function () use ($store, $profile, $operation, $operationId): array {
            if ($store->read('execution') !== null) {
                throw new RuntimeException('Profile execution requires stopped recovery.');
            }

            $this->recover($store, $profile);
            if (($store->read('completed')['operation_id'] ?? null) === $operationId) {
                throw new RuntimeException('A completed operation cannot be replayed.');
            }
            $pending = [
                'profile_id' => $profile,
                'operation_id' => $operationId,
                'operation' => $operation,
                'sandbox' => 'shipmunk-profile-'.$operationId,
            ];
            // Persist before begin: a lost response must not leave an unacknowledged server lease.
            $store->write('pending', $pending);
            try {
                $binding = $this->begin($profile, $pending);
            } catch (ControlPlaneException $exception) {
                if (in_array($exception->status, [401, 403, 404, 409, 422, 426], true)) {
                    $store->forget('pending');
                }
                throw $exception;
            }
            try {
                $this->validateBinding($binding, $profile, $operationId);
            } catch (Throwable $exception) {
                // A historical/terminal operation may be replayed after a newer login.
                // Its rejection must never invalidate the newer credential home.
                $this->rejectBegin($store, $profile, $pending, $binding);
                throw $exception;
            }
            $pending['binding'] = $binding;
            $store->write('pending', $pending);

            $watchdog = null;
            $health = [
                'health' => 'error',
                'reason' => 'operation_failed',
            ];
            $stage = 'credential_invalidation';
            try {
                if ($operation === 'disconnect') {
                    $store->invalidate();
                    $health = [
                        'health' => 'disconnected',
                        'reason' => 'disconnected',
                    ];
                } else {
                    $health = $this->authenticate($store, $pending, $binding, $watchdog, $stage);
                }
            } catch (Throwable) {
                // Native and transport errors may contain secrets. Report only the internal stage.
                $this->reportFailure($stage);
                $health = [
                    'health' => 'error',
                    'reason' => 'operation_failed',
                ];
            }

            // If cleanup fails, retain the journal and exclusion. Never falsely acknowledge stopped.
            $this->runtime->stop($pending['sandbox']);
            $watchdog?->disarm();
            if ($health['health'] !== 'ready') {
                $store->invalidate();
            }
            $pending['outcome'] = $health;
            $store->write('pending', $pending);
            $response = $this->complete($profile, $pending, $health);
            if ($health['health'] === 'ready'
                && ($response['active'] ?? false) === true) {
                $store->write('active', $this->identity($binding));
            } else {
                $store->invalidate();
                if ($health['health'] === 'ready') {
                    $health = [
                        'health' => 'disconnected',
                        'reason' => 'operation_stopped',
                    ];
                }
            }
            $store->write('completed', ['operation_id' => $operationId]);
            $store->forget('pending');

            return $health;
        });
    }

    private function recover(
        ProfileStore $store,
        string $profile,
    ): void {
        $pending = $store->read('pending');
        if ($pending === null) {
            return;
        }
        if (($pending['profile_id'] ?? null) !== $profile) {
            throw new RuntimeException('Profile journal identity mismatch.');
        }
        ProfileStore::identifier($pending['operation_id']);
        if ($pending['sandbox'] !== 'shipmunk-profile-'.$pending['operation_id']) {
            throw new RuntimeException('Profile journal sandbox mismatch.');
        }
        $this->runtime->stop($pending['sandbox']);
        if (! isset($pending['binding'])) {
            $pending['binding'] = $this->begin($profile, $pending);
            try {
                $this->validateBinding($pending['binding'], $profile, $pending['operation_id']);
            } catch (Throwable $exception) {
                $this->rejectBegin($store, $profile, $pending, $pending['binding']);
                throw $exception;
            }
        }
        $outcome = $pending['outcome'] ?? [
            'health' => 'error',
            'reason' => 'operation_stopped',
        ];
        if ($outcome['health'] === 'ready') {
            // The response may have been lost after the server committed ready.
            // Retry exactly that completion before deciding whether activation is authorized.
            try {
                $store->validateHome();
            } catch (Throwable) {
                $outcome = [
                    'health' => 'error',
                    'reason' => 'operation_stopped',
                ];
                $pending['outcome'] = $outcome;
                $pending['recovery_failed'] = true;
                $store->write('pending', $pending);
                $store->invalidate();
            }
        } else {
            $store->invalidate();
        }
        try {
            $response = $this->complete($profile, $pending, $outcome);
        } catch (ControlPlaneException $exception) {
            if ($exception->status !== 404) {
                throw $exception;
            }
            // A missing operation can no longer reserve the runner's profile.
            $response = [
                'stopped' => true,
                'active' => false,
                'health' => 'disconnected',
            ];
        }
        if (($pending['recovery_failed'] ?? false) === true
            && (($response['stopped'] ?? false) !== true
                || ($response['active'] ?? true) !== false
                || ! in_array($response['health'] ?? null, ['error', 'disconnected'], true))) {
            // An idempotent 200 may describe the earlier ready commit. It does not
            // acknowledge missing credentials; retain the journal until product disconnect.
            throw new RuntimeException('Failed credential recovery requires confirmed profile disconnection.');
        }
        if ($outcome['health'] === 'ready'
            && ($response['active'] ?? false) === true) {
            $store->write('active', $this->identity($pending['binding']));
        } else {
            $store->invalidate();
        }
        $store->write('completed', ['operation_id' => $pending['operation_id']]);
        $store->forget('pending');
    }

    private function rejectBegin(
        ProfileStore $store,
        string $profile,
        array $pending,
        array $binding,
    ): void {
        if (($binding['profile_id'] ?? null) !== $profile
            || ($binding['operation_id'] ?? null) !== $pending['operation_id']) {
            return; // Unknown response ownership: retain recovery state.
        }
        if (($binding['stopped'] ?? null) === true) {
            $store->forget('pending');

            return;
        }
        if (($binding['stopped'] ?? null) === false
            && is_string($binding['runtime_version'] ?? null)) {
            $pending['binding'] = $binding;
            $store->write('pending', $pending);
            $this->runtime->stop($pending['sandbox']);
            $store->invalidate();
            $this->complete($profile, $pending, [
                'health' => 'error',
                'reason' => 'operation_stopped',
            ]);
            $store->write('completed', ['operation_id' => $pending['operation_id']]);
            $store->forget('pending');
        }
    }

    private function begin(
        string $profile,
        array $pending,
    ): array {
        return $this->controlPlane->request($profile, 'operations', [
            'operation' => $pending['operation'],
            'operation_id' => $pending['operation_id'],
        ]);
    }

    private function complete(
        string $profile,
        array $pending,
        array $health,
    ): array {
        if (! is_string($pending['binding']['runtime_version'] ?? null)) {
            throw new RuntimeException('Profile runtime version is missing.');
        }

        return $this->controlPlane->request($profile, 'operations/'.$pending['operation_id'].'/completion', [
            'stopped' => true,
            ...$health,
            'runtime_version' => $pending['binding']['runtime_version'],
        ]);
    }

    private function validateBinding(
        array $binding,
        string $profile,
        string $operationId,
    ): void {
        if (($binding['profile_id'] ?? null) !== $profile
            || ($binding['operation_id'] ?? null) !== $operationId
            || ($binding['stop_requested'] ?? null) !== false
            || ! is_string($binding['runtime_version'] ?? null)
            || ! is_string($binding['lease_expires_at'] ?? null)
            || (new DateTimeImmutable($binding['lease_expires_at']))->getTimestamp() <= time()) {
            throw new RuntimeException('Profile operation is no longer authorized.');
        }
    }

    private function identity(array $binding): array
    {
        return NativeProfile::identity($binding);
    }

    private function reportFailure(string $stage): void
    {
        try {
            $this->diagnostic?->__invoke($stage);
        } catch (Throwable) {
            // Diagnostics must never prevent process cleanup or the stopped acknowledgement.
        }
    }

    private function authenticate(
        ProfileStore $store,
        array $pending,
        array $binding,
        ?WatchdogLease &$watchdog,
        string &$stage,
    ): array {
        $agent = $binding['agent'] ?? null;
        if (($binding['auth_mode'] ?? null) !== 'subscription') {
            return [
                'health' => 'unsupported',
                'reason' => 'native_mode_mismatch',
            ];
        }
        if (! is_string($agent)
            || ($binding['runtime_version'] ?? null) !== (NativeProfile::VERSIONS[$agent] ?? null)) {
            return [
                'health' => 'unsupported',
                'reason' => 'runtime_mismatch',
            ];
        }
        $stage = 'credential_reference';
        if (! is_string($binding['credential_reference'] ?? null)
            || preg_match('/^credential:[A-Za-z0-9_-]+$/D', $binding['credential_reference']) !== 1) {
            throw new RuntimeException('Invalid credential reference.');
        }
        $stage = 'credential_home_preparation';
        if ($pending['operation'] === 'login') {
            $store->invalidate();
            NativeProfile::initializeHome($store->createHome());
        } elseif ($this->identity($store->read('active') ?? []) !== $this->identity($binding)) {
            return [
                'health' => 'expired',
                'reason' => 'native_login_required',
            ];
        }
        // Invalidate access while native login/status may rotate credentials.
        $store->forget('active');
        $stage = 'credential_home_validation';
        $store->validateHome();
        $lease = new DateTimeImmutable($binding['lease_expires_at']);
        $stage = 'sandbox_start';
        $this->runtime->start($pending['sandbox'], $store->home());
        $stage = 'watchdog_arm';
        $watchdog = $this->watchdog->arm($pending['sandbox'], $lease, new DateTimeImmutable('+15 minutes'));
        $nextHeartbeat = 0;
        $checkpoint = function () use ($pending, $binding, $watchdog, &$lease, &$nextHeartbeat, &$stage): void {
            $previousStage = $stage;
            $stage = 'heartbeat';
            if (time() >= $lease->getTimestamp()) {
                throw new RuntimeException('Profile operation lease expired.');
            }
            if (hrtime(true) < $nextHeartbeat) {
                $stage = $previousStage;

                return;
            }
            $renewal = $this->controlPlane->request($binding['profile_id'], 'operations/'.$pending['operation_id'].'/heartbeat', []);
            $this->validateBinding($renewal, $binding['profile_id'], $pending['operation_id']);
            if ($this->identity($renewal) !== $this->identity($binding)) {
                throw new RuntimeException('Profile binding changed during native authentication.');
            }
            $lease = new DateTimeImmutable($renewal['lease_expires_at']);
            $watchdog->renew($lease);
            $nextHeartbeat = hrtime(true) + 10_000_000_000;
            $stage = $previousStage;
        };
        $checkpoint();
        $stage = 'native_version';
        $version = $this->runtime->run($pending['sandbox'], $agent, 'version', $checkpoint);
        if (! NativeProfile::versionMatches($agent, $binding['runtime_version'], $version)) {
            return [
                'health' => 'unsupported',
                'reason' => 'runtime_mismatch',
            ];
        }
        if ($pending['operation'] === 'login') {
            $stage = 'native_login';
            $login = $this->runtime->run($pending['sandbox'], $agent, 'login', $checkpoint);
            if ($login->exitCode !== 0) {
                return [
                    'health' => 'expired',
                    'reason' => 'native_login_required',
                ];
            }
        }
        $stage = 'native_probe';
        $health = NativeProfile::health($agent, $this->runtime->run($pending['sandbox'], $agent, 'probe', $checkpoint));
        if ($health['health'] === 'ready') {
            $stage = 'native_preflight';
            $preflight = $this->runtime->run($pending['sandbox'], $agent, 'preflight', $checkpoint);
            $health = NativeProfile::authenticatedHealth($agent, $preflight);
            if ($health['health'] === 'ready') {
                // Native use may refresh credentials: establish billing mode again afterward.
                $stage = 'post_auth_probe';
                $health = NativeProfile::health($agent, $this->runtime->run($pending['sandbox'], $agent, 'probe', $checkpoint));
            }
        }
        // Last fresh authorization after native output and before the completion handshake.
        $nextHeartbeat = 0;
        $checkpoint();
        $stage = 'sandbox_stop';
        $this->runtime->stop($pending['sandbox']);
        $stage = 'post_auth_home_validation';
        $store->normalizeNativeHome();

        return $health;
    }
}
