<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use DateTimeImmutable;
use RuntimeException;

final class WatchdogHandle implements WatchdogLease
{
    /**
     * @var list<resource>
     */
    private static array $channels = [];

    /**
     * @var resource|null
     */
    private mixed $socket;

    /**
     * @param  resource  $socket
     */
    public function __construct(
        private readonly int $pid,
        mixed $socket,
    ) {
        $this->socket = $socket;
        self::$channels = array_values(array_filter(self::$channels, is_resource(...)));
        self::$channels[] = $socket;
    }

    /**
     * Sibling watchdogs must not keep the parent's renewal channels alive after its death.
     */
    public static function closeInheritedChannels(): void
    {
        foreach (self::$channels as $channel) {
            if (is_resource($channel)) {
                fclose($channel);
            }
        }

        self::$channels = [];
    }

    public function renew(DateTimeImmutable $lease): void
    {
        $this->send('renew:'.$lease->getTimestamp());
    }

    public function disarm(): void
    {
        if (is_resource($this->socket)) {
            $socket = $this->socket;

            try {
                $this->send('disarm');
            } catch (RuntimeException) {
                // Parent has already confirmed the sandbox absent; reap an expired watchdog.
            }

            fclose($socket);
            $this->socket = null;
        }

        pcntl_waitpid($this->pid, $status);
    }

    private function send(string $message): void
    {
        $wire = $message."\n";

        if (! is_resource($this->socket) || @fwrite($this->socket, $wire) !== strlen($wire)) {
            throw new RuntimeException('Independent host watchdog is unavailable.');
        }

        fflush($this->socket);
    }
}
