<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

interface NativeExecutableAdapter
{
    public function decode(Claim $claim, int $exitCode, string $output): NativeExecution;
}
