<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use JsonException;
use RuntimeException;

final class NormalizedExecutionAdapter implements NativeExecutableAdapter
{
    public function decode(
        Claim $claim,
        int $exitCode,
        string $output,
    ): NativeExecution {
        try {
            $decoded = json_decode(trim($output), true, flags: JSON_THROW_ON_ERROR);
        } catch (JsonException $exception) {
            throw new RuntimeException('Native executable returned malformed output.', previous: $exception);
        }

        if (! is_array($decoded)) {
            throw new RuntimeException('Native executable omitted its normalized result.');
        }

        $result = $this->result($decoded['result'] ?? null);

        if (($result['protocol_version'] ?? null) !== HttpControlPlaneClient::PROTOCOL_VERSION
            || ($result['run_id'] ?? null) !== $claim->runId
            || ($result['attempt_id'] ?? null) !== $claim->attemptId
            || ($result['fence'] ?? null) !== $claim->fence) {
            throw new RuntimeException('Native executable result did not match the active fence.');
        }

        if ($exitCode !== 0) {
            $result['outcome'] = 'incomplete';
            $result['summary'] = 'Native executable exited unsuccessfully.';
            $result['findings'] = [];
            $result['patch_artifact'] = null;
            $result['tests'] = is_array($result['tests'] ?? null) ? $result['tests'] : [];
            $result['usage'] = null;
        } else {
            $this->validateResult($result);
        }

        return new NativeExecution(
            $this->events($decoded['events'] ?? []),
            $this->artifacts($decoded['artifacts'] ?? []),
            $result,
        );
    }

    /**
     * @return array<string, mixed>
     */
    private function result(mixed $rawResult): array
    {
        if (! is_array($rawResult)) {
            throw new RuntimeException('Native executable omitted its normalized result.');
        }

        $result = [];

        foreach ($rawResult as $key => $value) {
            if (! is_string($key)) {
                throw new RuntimeException('Native executable result must be an object.');
            }

            $result[$key] = $value;
        }

        return $result;
    }

    /**
     * @return list<array<string, mixed>>
     */
    private function events(mixed $rawEvents): array
    {
        if (! is_array($rawEvents) || ! array_is_list($rawEvents)) {
            throw new RuntimeException('Native executable events are invalid.');
        }

        $events = [];

        foreach ($rawEvents as $event) {
            if (! is_array($event)) {
                throw new RuntimeException('Native executable event is invalid.');
            }

            $typedEvent = [];

            foreach ($event as $key => $value) {
                if (! is_string($key)) {
                    throw new RuntimeException('Native executable event must be an object.');
                }

                $typedEvent[$key] = $value;
            }

            $events[] = $typedEvent;
        }

        return $events;
    }

    /**
     * @return list<array{kind: string, bytes: string, sha256: string}>
     */
    private function artifacts(mixed $rawArtifacts): array
    {
        if (! is_array($rawArtifacts) || ! array_is_list($rawArtifacts)) {
            throw new RuntimeException('Native executable artifacts are invalid.');
        }

        $artifacts = [];

        foreach ($rawArtifacts as $artifact) {
            if (! is_array($artifact)
                || ! is_string($artifact['kind'] ?? null)
                || ! is_string($artifact['bytes'] ?? null)
                || ! is_string($artifact['sha256'] ?? null)) {
                throw new RuntimeException('Native executable artifact is invalid.');
            }

            $artifacts[] = [
                'kind' => $artifact['kind'],
                'bytes' => $artifact['bytes'],
                'sha256' => $artifact['sha256'],
            ];
        }

        return $artifacts;
    }

    /**
     * @param  array<string, mixed>  $result
     */
    private function validateResult(array $result): void
    {
        $outcome = $result['outcome'] ?? null;
        $validOutcome = is_string($outcome) && in_array($outcome, [
            'findings',
            'no_findings',
            'changes_proposed',
            'incomplete',
            'needs_input',
        ], true);

        if (! is_string($result['summary'] ?? null)
            || $result['summary'] === ''
            || ! $validOutcome
            || ! is_array($result['findings'] ?? null)
            || ! is_array($result['tests'] ?? null)
            || (! is_array($result['patch_artifact'] ?? null) && ($result['patch_artifact'] ?? null) !== null)
            || (! is_array($result['usage'] ?? null) && ($result['usage'] ?? null) !== null)) {
            throw new RuntimeException('Native executable result is malformed.');
        }
    }
}
