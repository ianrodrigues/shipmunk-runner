<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

interface Clock
{
    public function now(): float;

    public function sleepMilliseconds(int $milliseconds): void;
}
