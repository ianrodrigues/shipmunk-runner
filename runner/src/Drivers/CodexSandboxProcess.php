<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Drivers;

use Closure;
use RuntimeException;
use Shipmunk\Runner\Claim;
use Shipmunk\Runner\NativeExecution;
use Shipmunk\Runner\Profiles\ProfileStore;
use Shipmunk\Runner\SandboxProcess;

final class CodexSandboxProcess implements SandboxProcess
{
    private ?DockerAgentTransport $transport = null;

    private ?NativeExecution $execution = null;

    public function __construct(
        private readonly string $name,
        private readonly Claim $claim,
        private readonly string $workspace,
        private readonly ProfileStore $profile,
        private readonly string $nativeImage,
        private readonly string $repositoryImage,
        private readonly AgentSessionStore $sessions,
        private readonly int $maxCommands,
    ) {}

    public function id(): string
    {
        return $this->name;
    }

    /**
     * @param  (Closure(int): void)|null  $checkpoint
     */
    public function start(?Closure $checkpoint = null): void
    {
        if ($checkpoint === null
            || $this->transport !== null) {
            throw new RuntimeException('Native execution requires a fresh supervised checkpoint.');
        }

        $this->transport = new DockerAgentTransport(
            $this->name,
            $this->profile->home(),
            $this->workspace,
            $this->nativeImage,
            $this->repositoryImage,
            $checkpoint,
            $this->maxCommands,
        );
        $driver = new CodexDriver((new TrustedAgentInstructions)->read($this->claim, $this->workspace));

        try {
            $this->transport->start();
            $driver->inspect($this->transport, $checkpoint);
            $driver->probe($this->transport, $checkpoint);
            $this->execution = $driver->start(
                $this->claim,
                $this->transport,
                $checkpoint,
                $this->sessions->read($this->claim),
            );

            if ($driver->lastSession !== null) {
                try {
                    $this->sessions->write($this->claim, $driver->lastSession);
                } catch (RuntimeException) {
                    // Resume metadata is optional; keep the completed execution available.
                }
            }
        } catch (DriverFailure $failure) {
            $this->execution = new NativeExecution([], [], [
                'protocol_version' => '1.0',
                'run_id' => $this->claim->runId,
                'attempt_id' => $this->claim->attemptId,
                'fence' => $this->claim->fence,
                'outcome' => $failure->reason === 'approval_required' ? 'needs_input' : 'incomplete',
                'summary' => $failure->getMessage(),
                'findings' => [],
                'tests' => [],
                'patch_artifact' => null,
                'usage' => null,
            ]);
        }
    }

    public function isRunning(): bool
    {
        return false;
    }

    public function exitCode(): ?int
    {
        return $this->execution === null ? null : 0;
    }

    public function output(): string
    {
        if ($this->execution === null) {
            throw new RuntimeException('Native execution has no normalized result.');
        }

        return json_encode([
            'events' => $this->execution->events,
            'artifacts' => $this->execution->artifacts,
            'result' => $this->execution->result,
        ], JSON_THROW_ON_ERROR);
    }

    public function stop(): void
    {
        ($this->transport ?? new DockerAgentTransport(
            $this->name, '', '', '', '', static function (): void {},
        ))->stop();
        $this->profile->normalizeNativeHome();
        $this->profile->validateHome();
    }

    public function remove(): void
    {
        $this->stop();
    }
}
