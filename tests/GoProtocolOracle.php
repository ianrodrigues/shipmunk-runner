<?php

declare(strict_types=1);

use Shipmunk\Runner\HttpControlPlaneClient;
use Shipmunk\Runner\HttpResponse;
use Shipmunk\Runner\HttpTransport;
use Shipmunk\Runner\Profiles\HttpProfileControlPlane;

require dirname(__DIR__).'/runner/bootstrap.php';

final class GoProtocolOracleTransport implements HttpTransport
{
    /** @var list<array{method: string, path: string, headers: array<string, string>, body: string}> */
    public array $requests = [];

    /** @param array<string, string> $responseHeaders */
    public function __construct(
        private readonly int $status,
        private readonly array $responseHeaders,
        private readonly string $responseBody,
    ) {}

    public function request(string $method, string $url, array $headers, string $body, int $timeoutSeconds, int $maxResponseBytes): HttpResponse
    {
        $path = (string) (parse_url($url, PHP_URL_PATH) ?? '');
        $normalizedHeaders = [];

        foreach ($headers as $name => $value) {
            $normalizedHeaders[strtolower($name)] = $value;
        }

        $this->requests[] = [
            'method' => $method,
            'path' => $path,
            'headers' => $normalizedHeaders,
            'body' => $body,
        ];

        return new HttpResponse($this->status, $this->responseHeaders, $this->responseBody);
    }
}

try {
    $input = json_decode(stream_get_contents(STDIN), true, flags: JSON_THROW_ON_ERROR);
    $response = $input['response'];
    $transport = new GoProtocolOracleTransport(
        $response['status'],
        $response['headers'],
        $response['body'],
    );
    $client = new HttpControlPlaneClient('https://control.example/base/', 'synthetic-token', $transport);
    $claimData = json_decode($input['manifest'], true, flags: JSON_THROW_ON_ERROR);

    switch ($input['operation']) {
        case 'claim':
            $client->claim();
            break;
        case 'heartbeat':
            $client->heartbeat(Shipmunk\Runner\Claim::fromArray($claimData));
            break;
        case 'stopped':
            $client->acknowledgeStopped(Shipmunk\Runner\Claim::fromArray($claimData));
            break;
        case 'events':
            $client->sendEvents(Shipmunk\Runner\Claim::fromArray($claimData), $input['payload']['events']);
            break;
        case 'upload':
            $client->uploadArtifact(
                Shipmunk\Runner\Claim::fromArray($claimData),
                $input['payload']['kind'],
                $input['payload']['body'],
                $input['payload']['sha256'],
            );
            break;
        case 'download':
            $client->downloadArtifact(
                Shipmunk\Runner\Claim::fromArray($claimData),
                $input['payload']['artifact_id'],
                $input['payload']['sha256'],
            );
            break;
        case 'completion':
            $client->complete(Shipmunk\Runner\Claim::fromArray($claimData), $input['payload']['result']);
            break;
        case 'profile':
            (new HttpProfileControlPlane('https://control.example/base/', 'synthetic-token', $transport))->request(
                $input['payload']['profile_id'],
                $input['payload']['suffix'],
                $input['payload']['payload'],
            );
            break;
        default:
            throw new RuntimeException('Unknown oracle operation.');
    }

    if (count($transport->requests) !== 1) {
        throw new RuntimeException('Oracle did not capture exactly one request.');
    }

    $request = $transport->requests[0];
    echo json_encode($request, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES);
} catch (Throwable $exception) {
    fwrite(STDERR, $exception::class.': '.$exception->getMessage()."\n");
    exit(1);
}
