<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Drivers;

use Closure;
use Shipmunk\Runner\CommandResult;

/**
 * Trusted native commands and repository operations must occupy separate process boundaries.
 */
interface AgentTransport
{
    /**
     * @param  list<string>  $argv
     * @param  Closure(int): void  $checkpoint
     */
    public function run(
        array $argv,
        string $stdin,
        Closure $checkpoint,
    ): CommandResult;

    public function patch(): ?string;

    /**
     * Return metadata derived from the protected original and frozen repository snapshots.
     *
     * @return list<array{path: string, before_sha256: ?string, after_sha256: ?string, before_mode: ?string, after_mode: ?string}>
     */
    public function changedFiles(): array;

    /**
     * Confirm the complete native and repository process trees are absent.
     */
    public function stop(): void;
}
