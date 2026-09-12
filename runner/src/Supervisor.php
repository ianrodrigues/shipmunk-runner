<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use DateTimeImmutable;
use RuntimeException;
use Shipmunk\Runner\Profiles\ProfileStore;

final readonly class Supervisor
{
    public const int HEARTBEAT_SECONDS = 10;

    public const int LEASE_SECONDS = 45;

    public function __construct(
        private ControlPlaneClient $client,
        private Sandbox $sandbox,
        private NativeExecutableAdapter $adapter,
        private AttemptStateStore $state,
        private WorkspacePreparer $workspaces,
        private AgentInputSanitizer $sanitizer = new AgentInputSanitizer,
        private Clock $clock = new SystemClock,
        private Watchdog $watchdog = new HostWatchdog,
    ) {}

    public function reconcileAfterRestart(): void
    {
        $state = $this->state->load();

        if ($state === null) {
            return;
        }

        $claim = new Claim(
            $state['run_id'],
            $state['attempt_id'],
            $state['fence'],
            new DateTimeImmutable($state['lease_expires_at']),
            new DateTimeImmutable($state['deadline']),
            [
                'protocol_version' => HttpControlPlaneClient::PROTOCOL_VERSION,
                'profile_id' => $state['profile_id'] ?? null,
            ],
        );

        if ($this->sandbox instanceof ProfiledSandbox) {
            $this->sandbox->reconcileProfileExecution($claim, $state['sandbox_id']);
        } elseif ($state['sandbox_id'] !== null) {
            $this->sandbox->reconcile($state['sandbox_id']);
        }

        $this->workspaces->remove($state['workspace']);
        $this->client->acknowledgeStopped($claim);
        $this->state->clear();
    }

    public function runOnce(): bool
    {
        $this->reconcileAfterRestart();
        $this->state->ensureWritable();
        $this->workspaces->ensureWritable();
        $claim = $this->client->claim();

        if ($claim === null) {
            return false;
        }

        if ($this->sandbox instanceof ProfiledSandbox) {
            $profileId = $claim->manifest['profile_id'] ?? null;

            if (! is_string($profileId)) {
                throw new RuntimeException('Native execution requires a valid profile identity.');
            }

            ProfileStore::identifier($profileId);

            $entered = false;
            $this->state->save($claim, null, $claim->leaseExpiresAt, $this->workspaces->path($claim));

            try {
                $this->sandbox->withProfile($claim, function () use ($claim): void {
                    $this->assertBeforeDeadline($claim);
                    $heartbeat = $this->client->heartbeat($claim);

                    if ($heartbeat->stop) {
                        throw new RuntimeException('Control plane requested execution stop.');
                    }
                }, function () use ($claim, &$entered): void {
                    $entered = true;
                    $this->runClaim($claim);
                });
            } catch (\Throwable $exception) {
                if (! $entered) {
                    $this->sandbox->reconcileProfileExecution($claim, null);
                    $this->client->acknowledgeStopped($claim);
                    $this->state->clear();
                }

                throw $exception;
            }
        } else {
            $this->runClaim($claim);
        }

        return true;
    }

    private function runClaim(Claim $claim): void
    {
        $workspace = $this->workspaces->path($claim);
        $lease = $claim->leaseExpiresAt;
        $process = null;
        $watchdog = null;
        $this->state->save($claim, null, $lease, $workspace);
        $nextHeartbeat = $this->clock->now();

        $checkpoint = function (int $upcomingSeconds = 0) use (
            $claim,
            &$process,
            &$watchdog,
            &$lease,
            &$nextHeartbeat,
            $workspace,
        ): void {
            $this->assertBeforeDeadline($claim);

            $now = $this->clock->now();

            if ($now >= $lease->getTimestamp()) {
                throw new RuntimeException('Lease expired locally.');
            }

            if ($now + $upcomingSeconds >= $nextHeartbeat) {
                $lease = $this->renewLease($claim, $process, $workspace, $watchdog);
                $nextHeartbeat = $now + self::HEARTBEAT_SECONDS;
            }
        };

        try {
            $lease = $this->renewLease($claim, null, $workspace, null);
            $nextHeartbeat = $this->clock->now() + self::HEARTBEAT_SECONDS;
            $this->workspaces->prepare($claim, $this->client, $checkpoint);
            $checkpoint(0);
            $process = $this->sandbox->create($claim, $this->sanitizer->sanitize($claim->manifest), $workspace);
            $this->state->save($claim, $process, $lease, $workspace);
            $watchdog = $this->watchdog->arm($process->id(), $lease, $claim->deadline);
            $lease = $this->renewLease($claim, $process, $workspace, $watchdog);
            $nextHeartbeat = $this->clock->now() + self::HEARTBEAT_SECONDS;
            $process->start($checkpoint);
            $checkpoint(0);

            $this->execute($claim, $process, $workspace, $watchdog);
        } catch (\Throwable $exception) {
            try {
                $this->cleanupAndAcknowledge($claim, $process, $workspace, $watchdog);
            } catch (\Throwable $cleanupException) {
                throw new RuntimeException('Sandbox cleanup acknowledgement failed; durable state was retained.', previous: $cleanupException);
            }

            throw $exception;
        }

        $this->cleanupAndAcknowledge($claim, $process, $workspace, $watchdog);
    }

    private function execute(
        Claim $claim,
        SandboxProcess $process,
        string $workspace,
        WatchdogLease $watchdog,
    ): void {
        $lease = new DateTimeImmutable($this->state->load()['lease_expires_at'] ?? throw new RuntimeException('Active lease state is missing.'));
        $startedAt = $this->clock->now();
        $nextHeartbeat = $startedAt + self::HEARTBEAT_SECONDS;

        while ($process->isRunning()) {
            $now = $this->clock->now();

            if ($now >= $lease->getTimestamp()) {
                throw new RuntimeException('Lease expired locally.');
            }

            $this->assertBeforeDeadline($claim);

            if ($now >= $nextHeartbeat) {
                $lease = $this->renewLease($claim, $process, $workspace, $watchdog);
                $nextHeartbeat = $now + self::HEARTBEAT_SECONDS;
            }

            $this->clock->sleepMilliseconds(100);
        }

        $exitCode = $process->exitCode();

        if ($exitCode === null) {
            throw new RuntimeException('Sandbox exited without an exit code.');
        }

        $execution = $this->adapter->decode($claim, $exitCode, $process->output());
        $patchArtifacts = array_values(array_filter(
            $execution->artifacts,
            fn (array $artifact): bool => $artifact['kind'] === 'patch',
        ));

        if (count($patchArtifacts) > 1) {
            throw new RuntimeException('Native execution produced multiple patch artifacts.');
        }

        if ($patchArtifacts === [] && ($execution->result['patch_artifact'] ?? null) !== null) {
            throw new RuntimeException('Native result referenced a patch that was not uploaded.');
        }

        foreach (array_chunk($execution->events, 100) as $events) {
            $this->renewLease($claim, $process, $workspace, $watchdog);
            $this->client->sendEvents($claim, $events);
        }

        $patch = null;

        foreach ($execution->artifacts as $artifact) {
            $this->renewLease($claim, $process, $workspace, $watchdog);
            $artifactId = $this->client->uploadArtifact(
                $claim,
                $artifact['kind'],
                $artifact['bytes'],
                $artifact['sha256'],
            );

            if ($artifact['kind'] === 'patch') {
                $patch = [
                    'artifact_id' => $artifactId,
                    'sha256' => $artifact['sha256'],
                ];
            }
        }

        $result = $execution->result;

        $result['patch_artifact'] = $patch;

        $this->renewLease($claim, $process, $workspace, $watchdog);
        $this->client->complete($claim, $result);
    }

    private function renewLease(
        Claim $claim,
        ?SandboxProcess $process,
        string $workspace,
        ?WatchdogLease $watchdog,
    ): DateTimeImmutable {
        $heartbeat = $this->client->heartbeat($claim);

        if ($heartbeat->stop) {
            throw new RuntimeException('Control plane requested execution stop.');
        }

        $this->state->save($claim, $process, $heartbeat->leaseExpiresAt, $workspace);
        $watchdog?->renew($heartbeat->leaseExpiresAt);

        return $heartbeat->leaseExpiresAt;
    }

    private function cleanupAndAcknowledge(
        Claim $claim,
        ?SandboxProcess $process,
        string $workspace,
        ?WatchdogLease $watchdog,
    ): void {
        if ($process !== null) {
            $process->stop();
            $process->remove();
        }

        if ($this->sandbox instanceof ProfiledSandbox) {
            $this->sandbox->releaseProfileExecution($claim);
        }

        $watchdog?->disarm();

        $this->workspaces->remove($workspace);
        $this->client->acknowledgeStopped($claim);
        $this->state->clear();
    }

    private function assertBeforeDeadline(Claim $claim): void
    {
        if ($this->clock->now() >= $claim->deadline->getTimestamp()) {
            throw new RuntimeException('Execution deadline expired.');
        }
    }
}
