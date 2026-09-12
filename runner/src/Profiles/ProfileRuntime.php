<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Profiles;

use Closure;
use Shipmunk\Runner\CommandResult;

interface ProfileRuntime
{
    public function start(string $sandbox, string $home): void;

    /** @param Closure(): void $checkpoint */
    public function run(string $sandbox, string $agent, string $command, Closure $checkpoint): CommandResult;

    /** Returns only after the entire native process tree is confirmed absent. */
    public function stop(string $sandbox): void;
}
