<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

interface HttpTransport
{
    /** @param array<string, string> $headers */
    public function request(string $method, string $url, array $headers, string $body, int $timeoutSeconds, int $maxResponseBytes): HttpResponse;
}
