<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use DateTimeImmutable;

final readonly class Heartbeat
{
    public function __construct(
        public bool $stop,
        public DateTimeImmutable $leaseExpiresAt,
    ) {}
}
