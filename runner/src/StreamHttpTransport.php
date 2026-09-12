<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use RuntimeException;

final class StreamHttpTransport implements HttpTransport
{
    public function request(string $method, string $url, array $headers, string $body, int $timeoutSeconds, int $maxResponseBytes): HttpResponse
    {
        $headerLines = [];

        foreach ($headers as $name => $value) {
            if (preg_match('/^[A-Za-z0-9-]+$/', $name) !== 1 || str_contains($value, "\r") || str_contains($value, "\n")) {
                throw new RuntimeException('Unsafe HTTP header.');
            }

            $headerLines[] = "{$name}: {$value}";
        }

        $context = stream_context_create(['http' => [
            'method' => $method,
            'header' => implode("\r\n", $headerLines),
            'content' => $body,
            'ignore_errors' => true,
            'timeout' => $timeoutSeconds,
            'follow_location' => 0,
        ]]);
        $stream = @fopen($url, 'rb', false, $context);

        if ($stream === false) {
            throw new RuntimeException('Control-plane request failed.');
        }

        $responseBody = stream_get_contents($stream, $maxResponseBytes + 1);
        $metadata = stream_get_meta_data($stream);
        fclose($stream);

        if ($responseBody === false || strlen($responseBody) > $maxResponseBytes) {
            throw new RuntimeException('Control-plane response exceeded its byte limit.');
        }

        $responseHeaders = $metadata['wrapper_data'] ?? [];
        $status = $this->status($responseHeaders);

        return new HttpResponse($status, $this->headers($responseHeaders), $responseBody);
    }

    private function status(mixed $headers): int
    {
        if (! is_array($headers)) {
            throw new RuntimeException('Control-plane response did not contain an HTTP status.');
        }

        foreach (array_reverse($headers) as $line) {
            if (is_string($line) && preg_match('/^HTTP\/\S+\s+(\d{3})/', $line, $matches) === 1) {
                return (int) $matches[1];
            }
        }

        throw new RuntimeException('Control-plane response did not contain an HTTP status.');
    }

    /**
     * @return array<string, string>
     */
    private function headers(mixed $headers): array
    {
        $parsed = [];

        if (! is_array($headers)) {
            return $parsed;
        }

        foreach ($headers as $line) {
            if (! is_string($line) || ! str_contains($line, ':')) {
                continue;
            }

            [$name, $value] = explode(':', $line, 2);
            $parsed[strtolower(trim($name))] = trim($value);
        }

        return $parsed;
    }
}
