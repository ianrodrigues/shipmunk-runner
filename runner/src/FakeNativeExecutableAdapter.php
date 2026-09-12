<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

final class FakeNativeExecutableAdapter implements NativeExecutableAdapter
{
    public function decode(
        Claim $claim,
        int $exitCode,
        string $output,
    ): NativeExecution {
        return (new NormalizedExecutionAdapter)->decode($claim, $exitCode, $output);
    }
}
