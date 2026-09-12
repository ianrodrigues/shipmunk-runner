<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

final readonly class HttpResponse
{
    /** @param array<string, string> $headers */
    public function __construct(
        public int $status,
        public array $headers,
        public string $body,
    ) {}
}
