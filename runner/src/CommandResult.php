<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

final readonly class CommandResult
{
    public function __construct(
        public int $exitCode,
        public string $stdout,
        public string $stderr,
    ) {}
}
