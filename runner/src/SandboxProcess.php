<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

interface SandboxProcess
{
    public function id(): string;

    /**
     * Start only after durable attempt state has been recorded.
     *
     * @param  (\Closure(int): void)|null  $checkpoint
     */
    public function start(?\Closure $checkpoint = null): void;

    public function isRunning(): bool;

    public function exitCode(): ?int;

    public function output(): string;

    /**
     * Send TERM to the sandbox process tree, wait, then escalate to KILL.
     */
    public function stop(): void;

    public function remove(): void;
}
