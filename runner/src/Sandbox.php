<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

interface Sandbox
{
    /** @param array<string, mixed> $agentInput */
    public function create(Claim $claim, array $agentInput, string $workspace): SandboxProcess;

    /** Confirm termination and remove any sandbox recorded before a supervisor restart. */
    public function reconcile(string $sandboxId): void;
}
