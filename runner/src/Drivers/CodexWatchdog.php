<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Drivers;

use DateTimeImmutable;
use Shipmunk\Runner\HostWatchdog;
use Shipmunk\Runner\Watchdog;
use Shipmunk\Runner\WatchdogLease;

final readonly class CodexWatchdog implements Watchdog
{
    public function __construct(private Watchdog $watchdog = new HostWatchdog) {}

    public function arm(
        string $sandboxId,
        DateTimeImmutable $lease,
        DateTimeImmutable $deadline,
    ): WatchdogLease {
        $leases = [];

        try {
            foreach ([$sandboxId, $sandboxId.'-repo', $sandboxId.'-diff'] as $container) {
                $leases[] = $this->watchdog->arm($container, $lease, $deadline);
            }
        } catch (\Throwable $exception) {
            foreach ($leases as $armed) {
                $armed->disarm();
            }

            throw $exception;
        }

        return new class($leases) implements WatchdogLease
        {
            /**
             * @param  list<WatchdogLease>  $leases
             */
            public function __construct(private array $leases) {}

            public function renew(DateTimeImmutable $lease): void
            {
                foreach ($this->leases as $armed) {
                    $armed->renew($lease);
                }
            }

            public function disarm(): void
            {
                $failure = null;

                foreach ($this->leases as $armed) {
                    try {
                        $armed->disarm();
                    } catch (\Throwable $exception) {
                        $failure ??= $exception;
                    }
                }

                if ($failure !== null) {
                    throw $failure;
                }
            }
        };
    }
}
