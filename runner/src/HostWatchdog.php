<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use DateTimeImmutable;
use RuntimeException;

final readonly class HostWatchdog implements Watchdog
{
    public function __construct(private CommandRunner $commands = new CommandRunner)
    {
        if (! extension_loaded('pcntl')) {
            throw new RuntimeException('The pcntl extension is required for the independent host watchdog.');
        }
    }

    public function arm(
        string $sandboxId,
        DateTimeImmutable $lease,
        DateTimeImmutable $deadline,
    ): WatchdogLease {
        $sockets = stream_socket_pair(STREAM_PF_UNIX, STREAM_SOCK_STREAM, STREAM_IPPROTO_IP);

        if ($sockets === false) {
            throw new RuntimeException('Unable to create watchdog control channel.');
        }

        $pid = pcntl_fork();

        if ($pid === -1) {
            fclose($sockets[0]);
            fclose($sockets[1]);

            throw new RuntimeException('Unable to start independent host watchdog.');
        }

        if ($pid === 0) {
            WatchdogHandle::closeInheritedChannels();
            fclose($sockets[0]);
            $this->monitor($sockets[1], $sandboxId, $lease, $deadline);
            exit(0);
        }

        fclose($sockets[1]);

        return new WatchdogHandle($pid, $sockets[0]);
    }

    /**
     * @param  resource  $socket
     */
    private function monitor(
        mixed $socket,
        string $sandboxId,
        DateTimeImmutable $lease,
        DateTimeImmutable $deadline,
    ): void {
        stream_set_blocking($socket, false);
        $buffer = '';
        $expiresAt = $this->monotonicExpiry($lease, $deadline);

        while (true) {
            $remainingNanoseconds = $expiresAt - hrtime(true);

            if ($remainingNanoseconds <= 0) {
                break;
            }

            $read = [$socket];
            $write = null;
            $except = null;
            $seconds = intdiv($remainingNanoseconds, 1_000_000_000);
            $microseconds = intdiv($remainingNanoseconds % 1_000_000_000, 1000);
            $selected = @stream_select($read, $write, $except, $seconds, $microseconds);

            if ($selected === false) {
                break;
            }

            if ($selected === 0) {
                continue;
            }

            $chunk = fread($socket, 8192);

            if ($chunk === '' || $chunk === false) {
                break;
            }

            $buffer .= $chunk;

            while (($newline = strpos($buffer, "\n")) !== false) {
                $message = substr($buffer, 0, $newline);
                $buffer = substr($buffer, $newline + 1);

                if ($message === 'disarm') {
                    fclose($socket);

                    return;
                }

                if (preg_match('/^renew:(\d+)$/', $message, $matches) === 1) {
                    $expiresAt = $this->monotonicExpiry(
                        new DateTimeImmutable('@'.$matches[1]),
                        $deadline,
                    );
                }
            }
        }

        fclose($socket);
        $this->removeSandbox($sandboxId);
    }

    private function monotonicExpiry(
        DateTimeImmutable $lease,
        DateTimeImmutable $deadline,
    ): int {
        $remainingSeconds = max(0, min(
            $lease->getTimestamp(),
            $deadline->getTimestamp(),
        ) - time());

        return hrtime(true) + ($remainingSeconds * 1_000_000_000);
    }

    private function removeSandbox(string $sandboxId): void
    {
        while (true) {
            try {
                $inspect = $this->commands->run(['docker', 'inspect', $sandboxId]);

                if ($inspect->exitCode !== 0) {
                    if ($this->sandboxIsAbsent($inspect)) {
                        return;
                    }

                    throw new RuntimeException('Unable to confirm watchdog sandbox state.');
                }

                $this->commands->run(['docker', 'stop', '--time', '2', $sandboxId]);
                $this->commands->run(['docker', 'kill', $sandboxId]);
                $this->commands->run(['docker', 'rm', '--force', $sandboxId]);

                $confirmation = $this->commands->run(['docker', 'inspect', $sandboxId]);

                if ($confirmation->exitCode !== 0 && $this->sandboxIsAbsent($confirmation)) {
                    return;
                }
            } catch (\Throwable) {
                // Keep retrying: durable state cannot prove isolation while this child is alive.
            }

            sleep(1);
        }
    }

    private function sandboxIsAbsent(CommandResult $result): bool
    {
        return preg_match('/no such (?:object|container)/i', $result->stdout.$result->stderr) === 1;
    }
}
