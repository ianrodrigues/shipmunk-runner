<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Drivers;

use Closure;
use RuntimeException;
use Shipmunk\Runner\Claim;
use Shipmunk\Runner\ProfiledSandbox;
use Shipmunk\Runner\Profiles\NativeProfile;
use Shipmunk\Runner\Profiles\ProfileExecutionGate;
use Shipmunk\Runner\Profiles\ProfileStore;
use Shipmunk\Runner\SandboxProcess;

final class CodexSandbox implements ProfiledSandbox
{
    private ?ProfileStore $profile = null;

    public function __construct(
        private readonly string $profilesDirectory,
        private readonly string $nativeImage,
        private readonly string $repositoryImage,
        private readonly AgentSessionStore $sessions,
        private readonly ?Closure $reconcileProcessTrees = null,
    ) {}

    public function withProfile(
        Claim $claim,
        Closure $authorize,
        Closure $work,
    ): void {
        $manifest = $claim->manifest;

        if (($manifest['agent'] ?? null) !== 'codex'
            || ($manifest['runtime_version'] ?? null) !== NativeProfile::VERSIONS['codex']
            || ! is_string($manifest['profile_id'] ?? null)
            || ! is_string($manifest['supervisor']['credential_reference'] ?? null)) {
            throw new RuntimeException('Claim requires an unsupported native profile.');
        }

        $store = new ProfileStore($this->profilesDirectory, $manifest['profile_id']);
        $binding = [
            'profile_id' => $manifest['profile_id'],
            'credential_reference' => $manifest['supervisor']['credential_reference'],
            'agent' => 'codex',
            'auth_mode' => 'subscription',
            'runtime_version' => NativeProfile::VERSIONS['codex'],
        ];

        (new ProfileExecutionGate)->execute($store, $binding, $authorize, function () use ($store, $claim, $work): void {
            $store->reserveExecution($claim);
            $this->profile = $store;

            try {
                $work();
            } finally {
                $this->profile = null;
            }
        });
    }

    public function create(
        Claim $claim,
        array $agentInput,
        string $workspace,
    ): SandboxProcess {
        if ($this->profile === null) {
            throw new RuntimeException('Native execution requires an active profile gate.');
        }

        $sources = $claim->manifest['source_artifacts'] ?? null;
        $maxCommands = $claim->manifest['effective_config']['max_turns'] ?? null;

        if (! is_array($sources)
            || ! in_array(count($sources), [1, 2], true)
            || ! is_int($maxCommands)
            || $maxCommands < 1
            || $maxCommands > 1000) {
            throw new RuntimeException('Native execution requires a head snapshot, optional base snapshot and bounded tool budget.');
        }

        return new CodexSandboxProcess(
            'shipmunk-codex-'.$claim->attemptId.'-'.$claim->fence,
            $claim,
            $workspace,
            $this->profile,
            $this->nativeImage,
            $this->repositoryImage,
            $this->sessions,
            $maxCommands,
        );
    }

    public function reconcile(string $sandboxId): void
    {
        if ($this->reconcileProcessTrees !== null) {
            ($this->reconcileProcessTrees)($sandboxId);

            return;
        }

        (new DockerAgentTransport(
            $sandboxId,
            '',
            '',
            '',
            '',
            static function (): void {},
        ))->stop();
    }

    public function releaseProfileExecution(Claim $claim): void
    {
        if ($this->profile === null) {
            throw new RuntimeException('Profile execution release requires the active gate.');
        }

        $this->profile->releaseExecution($claim);
    }

    public function reconcileProfileExecution(
        Claim $claim,
        ?string $sandboxId,
    ): void {
        $profileId = $claim->manifest['profile_id'] ?? null;

        if (! is_string($profileId)) {
            throw new RuntimeException('Native restart recovery requires a recorded profile.');
        }

        $expectedSandbox = 'shipmunk-codex-'.$claim->attemptId.'-'.$claim->fence;

        if ($sandboxId !== null
            && $sandboxId !== $expectedSandbox) {
            throw new RuntimeException('Native restart sandbox does not match the attempt.');
        }

        $store = new ProfileStore($this->profilesDirectory, $profileId);
        $store->exclusively(function () use ($store, $claim, $expectedSandbox): void {
            $store->assertExecutionMatches($claim);
            $this->reconcile($expectedSandbox);
            $store->releaseExecution($claim);
        });
    }
}
