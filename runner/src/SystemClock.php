<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

final class SystemClock implements Clock
{
    public function now(): float
    {
        return microtime(true);
    }

    public function sleepMilliseconds(int $milliseconds): void
    {
        usleep($milliseconds * 1000);
    }
}
