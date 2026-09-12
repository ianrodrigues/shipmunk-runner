<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use DateTimeImmutable;

interface WatchdogLease
{
    public function renew(DateTimeImmutable $lease): void;

    public function disarm(): void;
}
