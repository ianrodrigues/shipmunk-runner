<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use Closure;

interface ProfiledSandbox extends Sandbox
{
    /**
     * Hold profile exclusion through work, process cleanup and stopped acknowledgement.
     */
    public function withProfile(
        Claim $claim,
        Closure $authorize,
        Closure $work,
    ): void;

    /**
     * Release durable profile exclusion after supervised process cleanup.
     */
    public function releaseProfileExecution(Claim $claim): void;

    /**
     * Confirm restart cleanup and release only the matching profile reservation.
     */
    public function reconcileProfileExecution(
        Claim $claim,
        ?string $sandboxId,
    ): void;
}
