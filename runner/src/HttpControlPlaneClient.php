<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use DateTimeImmutable;
use InvalidArgumentException;
use JsonException;
use RuntimeException;

final readonly class HttpControlPlaneClient implements ControlPlaneClient
{
    public const string PROTOCOL_VERSION = Protocol::VERSION;

    public function __construct(
        private string $baseUrl,
        private string $token,
        private HttpTransport $transport = new StreamHttpTransport,
    ) {
        $parts = parse_url($baseUrl);
        $host = strtolower((string) ($parts['host'] ?? ''));
        $local = in_array($host, ['localhost', '127.0.0.1', '::1'], true);

        if (($parts['scheme'] ?? null) !== 'https' && ! (($parts['scheme'] ?? null) === 'http' && $local)) {
            throw new InvalidArgumentException('The control plane must use HTTPS outside local development.');
        }

        if ($token === '' || str_contains($token, "\r") || str_contains($token, "\n")) {
            throw new InvalidArgumentException('Runner token is invalid.');
        }
    }

    public function claim(): ?Claim
    {
        $response = $this->json('POST', '/runner/v1/claims', [
            'protocol_version' => self::PROTOCOL_VERSION,
        ]);

        if ($response->status === 204) {
            return null;
        }

        $data = $this->data($response, [200, 201]);

        return Claim::fromArray($data);
    }

    public function heartbeat(Claim $claim): Heartbeat
    {
        $data = $this->data($this->json('POST', "/runner/v1/attempts/{$claim->attemptId}/heartbeat", $this->fence($claim)), [200]);
        $lease = $data['lease_expires_at'] ?? null;

        if (! is_string($lease)) {
            throw new RuntimeException('Heartbeat lease is missing.');
        }

        if (($data['protocol_version'] ?? null) !== self::PROTOCOL_VERSION
            || ($data['attempt_id'] ?? null) !== $claim->attemptId
            || ($data['fence'] ?? null) !== $claim->fence) {
            throw new RuntimeException('Heartbeat response did not match the active fence.');
        }

        return new Heartbeat(($data['stop_requested'] ?? false) === true, new DateTimeImmutable($lease));
    }

    public function acknowledgeStopped(Claim $claim): void
    {
        $payload = $this->fence($claim);
        $payload['stopped'] = true;

        $this->expect($this->json('POST', "/runner/v1/attempts/{$claim->attemptId}/heartbeat", $payload), [200, 204]);
    }

    public function sendEvents(Claim $claim, array $events): void
    {
        if ($events === [] || count($events) > 100) {
            throw new InvalidArgumentException('Event batches must contain between 1 and 100 events.');
        }

        foreach ($events as $event) {
            if (strlen(json_encode($event, JSON_THROW_ON_ERROR)) > 65_536) {
                throw new InvalidArgumentException('An event exceeded 64 KiB.');
            }
        }

        $this->expect($this->json('POST', "/runner/v1/attempts/{$claim->attemptId}/events", $events), [200, 204]);
    }

    public function downloadArtifact(Claim $claim, string $artifactId, string $sha256): string
    {
        $response = $this->request(
            'GET',
            "/runner/v1/attempts/{$claim->attemptId}/input-artifacts/{$artifactId}",
            ['Accept' => 'application/octet-stream'],
            '',
            Protocol::INPUT_ARTIFACT_MAX_BYTES,
        );
        $this->expect($response, [200]);

        $headerHash = strtolower($response->headers['x-artifact-sha256'] ?? '');
        $contentLength = $response->headers['content-length'] ?? '';

        if (! ctype_digit($contentLength)
            || (int) $contentLength !== strlen($response->body)
            || ! hash_equals($sha256, $headerHash)
            || ! hash_equals($sha256, hash('sha256', $response->body))) {
            throw new RuntimeException('Downloaded artifact hash did not match the claim.');
        }

        return $response->body;
    }

    public function uploadArtifact(Claim $claim, string $kind, string $bytes, string $sha256): string
    {
        if (! hash_equals($sha256, hash('sha256', $bytes))) {
            throw new InvalidArgumentException('Upload artifact hash is invalid.');
        }

        $response = $this->request(
            'POST',
            "/runner/v1/attempts/{$claim->attemptId}/artifacts",
            [
                'Content-Type' => 'application/octet-stream',
                'X-Artifact-Kind' => $kind,
                'X-Artifact-SHA256' => $sha256,
                'X-Attempt-Fence' => (string) $claim->fence,
                'X-Protocol-Version' => self::PROTOCOL_VERSION,
            ],
            $bytes,
        );
        $data = $this->data($response, [200, 201]);
        $artifactId = $data['id'] ?? null;

        if (! is_string($artifactId)) {
            throw new RuntimeException('Artifact response is missing its identifier.');
        }

        return $artifactId;
    }

    public function complete(Claim $claim, array $result): void
    {
        $this->expect($this->json(
            'POST',
            "/runner/v1/attempts/{$claim->attemptId}/completion",
            array_merge($result, $this->fence($claim)),
            2 * 1024 * 1024,
        ), [200, 204]);
    }

    /** @return array<string, mixed> */
    private function fence(Claim $claim): array
    {
        return [
            'protocol_version' => self::PROTOCOL_VERSION,
            'attempt_id' => $claim->attemptId,
            'fence' => $claim->fence,
        ];
    }

    /** @param array<mixed> $payload */
    private function json(string $method, string $path, array $payload, int $maxBytes = 131_072): HttpResponse
    {
        return $this->request(
            $method,
            $path,
            ['Content-Type' => 'application/json'],
            json_encode($payload, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES),
            $maxBytes,
        );
    }

    /** @param array<string, string> $headers */
    private function request(string $method, string $path, array $headers, string $body, int $maxBytes = 131_072): HttpResponse
    {
        $headers['Accept'] ??= 'application/json';
        $headers['Authorization'] = 'Bearer '.$this->token;
        $headers['X-Shipmunk-Protocol'] = self::PROTOCOL_VERSION;

        return $this->transport->request(
            $method,
            rtrim($this->baseUrl, '/').$path,
            $headers,
            $body,
            Protocol::HTTP_TIMEOUT_SECONDS,
            $maxBytes,
        );
    }

    /** @param list<int> $statuses */
    private function expect(HttpResponse $response, array $statuses): void
    {
        if (! in_array($response->status, $statuses, true)) {
            $message = $response->status === 426
                ? 'Runner protocol major is incompatible.'
                : "Control-plane request returned HTTP {$response->status}.";

            throw new ControlPlaneException($response->status, $message);
        }
    }

    /**
     * @param  list<int>  $statuses
     * @return array<string, mixed>
     */
    private function data(HttpResponse $response, array $statuses): array
    {
        $this->expect($response, $statuses);

        try {
            $decoded = json_decode($response->body, true, flags: JSON_THROW_ON_ERROR);
        } catch (JsonException $exception) {
            throw new RuntimeException('Control-plane response was not valid JSON.', previous: $exception);
        }

        $rawData = is_array($decoded) ? ($decoded['data'] ?? null) : null;

        if (! is_array($rawData)) {
            throw new RuntimeException('Control-plane response data is missing.');
        }

        $data = [];

        foreach ($rawData as $key => $value) {
            if (! is_string($key)) {
                throw new RuntimeException('Control-plane response data must be an object.');
            }

            $data[$key] = $value;
        }

        return $data;
    }
}
