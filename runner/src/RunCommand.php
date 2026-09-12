<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use Closure;
use RuntimeException;
use Throwable;

final readonly class RunCommand
{
    /**
     * @param (Closure(string): void)|null $output
     * @param (Closure(string): void)|null $error
     */
    public function __construct(
        private Supervisor $supervisor,
        private ?Closure $output = null,
        private ?Closure $error = null,
    ) {}

    public function run(bool $once): int
    {
        $exitCode = 0;

        try {
            do {
                $worked = $this->supervisor->runOnce(function (
                    Claim $claim,
                    string $outcome,
                ) use (&$exitCode): void {
                    $exitCode = match ($outcome) {
                        'findings', 'no_findings', 'changes_proposed' => 0,
                        'incomplete', 'needs_input' => 1,
                        default => throw new RuntimeException('The completed run has an unsupported outcome.'),
                    };

                    $this->write('Run '.$claim->runId.', attempt '.$claim->attemptId.': '.$outcome.'.');
                });

                if (! $worked && $once) {
                    $this->write('No eligible queued work was returned for this runner.');
                }

                if (! $worked && ! $once) {
                    sleep(2);
                }
            } while (! $once);
        } catch (ControlPlaneException $exception) {
            $this->write(
                'Runner request failed with HTTP '.$exception->status.'. Check the runner connection and authorization.',
                true,
            );

            return 1;
        } catch (Throwable) {
            $this->write(
                'Runner stopped before a completed result could be reported. Check the application run and runner configuration.',
                true,
            );

            return 1;
        }

        return $exitCode;
    }

    private function write(
        string $message,
        bool $failed = false,
    ): void {
        $writer = $failed ? $this->error : $this->output;

        if ($writer !== null) {
            $writer($message."\n");

            return;
        }

        fwrite($failed ? STDERR : STDOUT, $message."\n");
    }
}
