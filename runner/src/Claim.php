<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use DateTimeImmutable;
use InvalidArgumentException;

final readonly class Claim
{
    /** @param array<string, mixed> $manifest */
    public function __construct(
        public string $runId,
        public string $attemptId,
        public int $fence,
        public DateTimeImmutable $leaseExpiresAt,
        public DateTimeImmutable $deadline,
        public array $manifest,
    ) {
        if ($fence < 1) {
            throw new InvalidArgumentException('Claim fence must be positive.');
        }

        foreach ([$runId, $attemptId] as $id) {
            if (preg_match('/^[0-7][0-9a-hjkmnp-tv-z]{25}$/', $id) !== 1) {
                throw new InvalidArgumentException('Claim identifiers must be lowercase ULIDs.');
            }
        }

        if (($manifest['protocol_version'] ?? null) !== HttpControlPlaneClient::PROTOCOL_VERSION) {
            throw new InvalidArgumentException('Unsupported manifest protocol version.');
        }
    }

    /** @param array<string, mixed> $data */
    public static function fromArray(array $data): self
    {
        return new self(
            self::string($data, 'run_id'),
            self::string($data, 'attempt_id'),
            self::integer($data, 'fence'),
            new DateTimeImmutable('+'.Supervisor::LEASE_SECONDS.' seconds'),
            new DateTimeImmutable(self::string($data, 'deadline')),
            $data,
        );
    }

    /** @param array<string, mixed> $data */
    private static function string(array $data, string $key): string
    {
        $value = $data[$key] ?? null;

        if (! is_string($value) || $value === '') {
            throw new InvalidArgumentException("Claim {$key} is invalid.");
        }

        return $value;
    }

    /** @param array<string, mixed> $data */
    private static function integer(array $data, string $key): int
    {
        $value = $data[$key] ?? null;

        if (! is_int($value)) {
            throw new InvalidArgumentException("Claim {$key} is invalid.");
        }

        return $value;
    }
}
