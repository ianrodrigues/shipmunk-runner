<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

final readonly class NativeExecution
{
    /**
     * @param  list<array<string, mixed>>  $events
     * @param  list<array{kind: string, bytes: string, sha256: string}>  $artifacts
     * @param  array<string, mixed>  $result
     */
    public function __construct(
        public array $events,
        public array $artifacts,
        public array $result,
    ) {}
}
