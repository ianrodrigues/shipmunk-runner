<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Profiles;

use RuntimeException;
use Shipmunk\Runner\ControlPlaneException;
use Shipmunk\Runner\HttpControlPlaneClient;
use Shipmunk\Runner\HttpTransport;
use Shipmunk\Runner\Protocol;
use Shipmunk\Runner\StreamHttpTransport;

final readonly class HttpProfileControlPlane implements ProfileControlPlane
{
    public function __construct(
        private string $baseUrl,
        private string $token,
        private HttpTransport $transport = new StreamHttpTransport,
    ) {
        // Reuse the supervisor's URL and token validation without loading the application.
        new HttpControlPlaneClient($baseUrl, $token, $transport);
    }

    public function request(string $profileId, string $suffix, array $payload): array
    {
        ProfileStore::identifier($profileId);

        if (preg_match('~^operations(?:/[0-9a-hjkmnp-tv-z]{26}/(?:heartbeat|completion))?$~D', $suffix) !== 1) {
            throw new RuntimeException('Invalid profile operation path.');
        }

        $response = $this->transport->request(
            'POST',
            rtrim($this->baseUrl, '/').'/runner/v1/profiles/'.$profileId.'/'.$suffix,
            [
                'Accept' => 'application/json',
                'Content-Type' => 'application/json',
                'Authorization' => 'Bearer '.$this->token,
                'X-Shipmunk-Protocol' => Protocol::VERSION,
            ],
            json_encode(['protocol_version' => Protocol::VERSION, ...$payload], JSON_THROW_ON_ERROR),
            Protocol::HTTP_TIMEOUT_SECONDS,
            32_768,
        );

        if (! in_array($response->status, [200, 201], true)) {
            throw new ControlPlaneException($response->status, 'Profile control-plane request failed.');
        }

        $decoded = json_decode($response->body, true, flags: JSON_THROW_ON_ERROR);
        $data = is_array($decoded) ? ($decoded['data'] ?? null) : null;

        if (! is_array($data) || ($data['profile_id'] ?? null) !== $profileId) {
            throw new RuntimeException('Invalid profile control-plane response.');
        }

        return $data;
    }
}
