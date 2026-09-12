<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use DateTimeImmutable;

interface Watchdog
{
    public function arm(string $sandboxId, DateTimeImmutable $lease, DateTimeImmutable $deadline): WatchdogLease;
}
