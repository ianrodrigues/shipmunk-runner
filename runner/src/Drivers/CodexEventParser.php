<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Drivers;

use JsonException;
use Shipmunk\Runner\Claim;
use Shipmunk\Runner\NativeExecution;
use stdClass;

/**
 * Bounded, fail-closed adapter for the pinned Codex exec JSONL transport.
 */
final class CodexEventParser
{
    public const MAX_OUTPUT_BYTES = 2_097_152;

    public const MAX_LINE_BYTES = 65_536;

    public const MAX_EVENTS = 10_000;

    public const MAX_PATCH_BYTES = 1_048_576;

    /**
     * Retain the first failure only after validating every record in the response.
     */
    public function preflightFailureReason(
        string $stdout,
        string $stderr = '',
    ): string {
        $reason = null;
        $terminal = false;

        foreach ($this->lines($stdout, $stderr) as $line) {
            if ($terminal) {
                throw new DriverFailure('malformed_output');
            }

            $event = $this->decode($line, 'malformed_output');
            $this->assertPreflightEvent($event);
            $type = $event->type;
            $itemFailure = str_starts_with($type, 'item.') && $event->item->type === 'error';
            $failure = $type === 'error' || $type === 'turn.failed' || $itemFailure;

            // Codex can report an error before turn.failed, but cannot resume work afterward.
            if ($reason !== null && ! $failure) {
                throw new DriverFailure('malformed_output');
            }

            $terminal = $type === 'turn.completed' || $type === 'turn.failed';

            if ($type === 'error' || $type === 'turn.failed') {
                $reason ??= $this->failureReason($type === 'error' ? $event : ($event->error ?? null));
            }

            if ($itemFailure) {
                $reason ??= $this->failureReason($event->item);
            }
        }

        return $reason ?? 'process_error';
    }

    private function assertPreflightEvent(stdClass $event): void
    {
        if (! in_array($event->type ?? null, [
            'thread.started',
            'turn.started',
            'turn.completed',
            'turn.failed',
            'error',
            'item.started',
            'item.updated',
            'item.completed',
        ], true)) {
            throw new DriverFailure('malformed_output');
        }

        if (in_array($event->type, ['item.started', 'item.updated', 'item.completed'], true)) {
            if (! ($event->item ?? null) instanceof stdClass) {
                throw new DriverFailure('malformed_output');
            }

            // Preflight disables tools; only messages, reasoning and failures are valid.
            if (! in_array($event->item->type ?? null, ['agent_message', 'reasoning', 'error'], true)) {
                throw new DriverFailure('malformed_output');
            }
        }
    }

    public function parse(
        Claim $claim,
        int $exitCode,
        string $stdout,
        string $stderr = '',
        ?string $patch = null,
    ): NativeExecution {
        $lines = $this->lines($stdout, $stderr);

        if ($stdout === '') {
            throw new DriverFailure($exitCode === 0 ? 'missing_result' : 'process_error');
        }

        $state = 'initial';
        $items = [];
        $events = [];
        $message = null;
        $usage = null;
        $structuredMessages = 0;

        foreach ($lines as $line) {
            if ($state === 'complete') {
                throw new DriverFailure('malformed_output');
            }

            $event = $this->decode($line, 'malformed_output');
            $type = $event->type ?? null;
            if ($type === 'error' || $type === 'turn.failed') {
                throw new DriverFailure($this->failureReason($type === 'error' ? $event : ($event->error ?? null)));
            }

            if ($type === 'thread.started' && $state === 'initial') {
                $this->text($event->thread_id ?? null, 128, 'malformed_output');
                $state = 'thread';
            } elseif ($type === 'turn.started' && $state === 'thread') {
                $state = 'turn';
                $events[] = $this->event($claim, count($events) + 1, 'progress', 'Codex execution started.');
            } elseif (in_array($type, ['item.started', 'item.updated', 'item.completed'], true) && $state === 'turn') {
                $item = $event->item ?? null;
                if (! $item instanceof stdClass) {
                    throw new DriverFailure('malformed_output');
                }
                $this->text($item->id ?? null, 128, 'malformed_output');
                $kind = $item->type ?? null;
                if (! in_array($kind, ['agent_message', 'reasoning', 'command_execution', 'file_change', 'mcp_tool_call', 'web_search', 'todo_list', 'error'], true)) {
                    throw new DriverFailure('malformed_output');
                }
                $previous = $items[$item->id] ?? null;
                if ($previous !== null && ($previous['complete'] || $previous['type'] !== $kind || $type === 'item.started')) {
                    throw new DriverFailure('malformed_output');
                }
                if ($type === 'item.updated' && $previous === null) {
                    throw new DriverFailure('malformed_output');
                }
                $items[$item->id] = ['type' => $kind, 'complete' => $type === 'item.completed'];
                if ($kind === 'error') {
                    throw new DriverFailure($this->failureReason($item));
                }
                if ($kind === 'agent_message' && $type === 'item.completed') {
                    $this->text($item->text ?? null, self::MAX_LINE_BYTES, 'invalid_result');
                    // Earlier commentary is allowed; only the last completed message is the final result.
                    if (str_starts_with(ltrim($item->text), '{') && ++$structuredMessages > 1) {
                        throw new DriverFailure('invalid_result');
                    }
                    $message = $item->text;
                }
                if (in_array($kind, ['command_execution', 'file_change', 'mcp_tool_call', 'web_search'], true) && $type !== 'item.updated') {
                    $events[] = $this->event($claim, count($events) + 1, $type === 'item.started' ? 'tool_started' : 'tool_finished', 'Codex tool activity.');
                }
            } elseif ($type === 'turn.completed' && $state === 'turn') {
                foreach ($items as $item) {
                    if (! $item['complete']) {
                        throw new DriverFailure('malformed_output');
                    }
                }
                $usage = $this->usage($event->usage ?? null);
                $state = 'complete';
            } else {
                throw new DriverFailure('malformed_output');
            }
        }

        if ($exitCode !== 0) {
            throw new DriverFailure('process_error');
        }
        if ($state !== 'complete' || $message === null) {
            throw new DriverFailure('missing_result');
        }

        $result = $this->result($message);
        $hasPatch = $patch !== null && $patch !== '';
        if (($hasPatch && strlen($patch) > self::MAX_PATCH_BYTES)
            || ($result['outcome'] === 'changes_proposed' && ! $hasPatch)
            || ($hasPatch && in_array($result['outcome'], ['findings', 'no_findings'], true))) {
            throw new DriverFailure('invalid_result');
        }

        $artifacts = [];
        $patchReference = null;
        if ($result['outcome'] === 'changes_proposed') {
            $sha256 = hash('sha256', $patch);
            $artifacts[] = ['kind' => 'patch', 'bytes' => $patch, 'sha256' => $sha256];
            $patchReference = ['artifact_id' => $claim->attemptId, 'sha256' => $sha256];
        }

        return new NativeExecution($events, $artifacts, [
            'protocol_version' => '1.0',
            'run_id' => $claim->runId,
            'attempt_id' => $claim->attemptId,
            'fence' => $claim->fence,
            ...$result,
            'patch_artifact' => $patchReference,
            'usage' => $usage,
        ]);
    }

    /**
     * @return list<string>
     */
    private function lines(
        string $stdout,
        string $stderr,
    ): array {
        if (strlen($stdout) + strlen($stderr) > self::MAX_OUTPUT_BYTES) {
            throw new DriverFailure('malformed_output');
        }

        if ($stdout === '') {
            return [];
        }

        if (! str_ends_with($stdout, "\n")) {
            throw new DriverFailure('malformed_output');
        }

        $lines = explode("\n", substr($stdout, 0, -1));

        if (count($lines) > self::MAX_EVENTS) {
            throw new DriverFailure('malformed_output');
        }

        foreach ($lines as $line) {
            if (strlen($line) > self::MAX_LINE_BYTES) {
                throw new DriverFailure('malformed_output');
            }
        }

        return $lines;
    }

    private function decode(
        string $json,
        string $reason,
    ): stdClass {
        try {
            $value = json_decode($json, false, 32, JSON_THROW_ON_ERROR);
        } catch (JsonException) {
            throw new DriverFailure($reason);
        }
        if (! $value instanceof stdClass) {
            throw new DriverFailure($reason);
        }

        $this->rejectDuplicateKeys($json, $reason);

        return $value;
    }

    /**
     * JSON decoding otherwise silently accepts duplicate authority or result keys.
     */
    private function rejectDuplicateKeys(
        string $json,
        string $reason,
    ): void {
        preg_match_all('/"(?:[^"\\\\]|\\\\.)*"|[{}\[\]:,]/s', $json, $matches);
        $stack = [];
        $tokens = $matches[0];
        foreach ($tokens as $index => $token) {
            if ($token === '{' || $token === '[') {
                $stack[] = [];
            } elseif ($token === '}' || $token === ']') {
                array_pop($stack);
            } elseif (str_starts_with($token, '"') && ($tokens[$index + 1] ?? null) === ':') {
                $key = json_decode($token, true, flags: JSON_THROW_ON_ERROR);
                $depth = count($stack) - 1;
                if (isset($stack[$depth][$key])) {
                    throw new DriverFailure($reason);
                }
                $stack[$depth][$key] = true;
            }
        }
    }

    /**
     * @return array<string, mixed>
     */
    private function result(string $message): array
    {
        $result = $this->decode($message, 'invalid_result');
        $this->keys($result, ['summary', 'outcome', 'findings', 'tests']);
        $this->text($result->summary, 16_384);
        if (! in_array($result->outcome, ['findings', 'no_findings', 'changes_proposed', 'incomplete', 'needs_input'], true)) {
            throw new DriverFailure('invalid_result');
        }
        foreach (['findings', 'tests'] as $field) {
            if (! is_array($result->$field) || count($result->$field) > 100) {
                throw new DriverFailure('invalid_result');
            }
        }
        if (($result->outcome === 'findings' && $result->findings === []) || ($result->outcome === 'no_findings' && $result->findings !== [])) {
            throw new DriverFailure('invalid_result');
        }
        foreach ($result->findings as $finding) {
            $this->keys($finding, ['path', 'line', 'side', 'severity', 'explanation', 'evidence']);
            $this->text($finding->path, 1024);
            if (preg_match('~^(?!/)(?!.*(?:^|/)\.\.(?:/|$))(?!.*\\\\)[^\x00-\x1f]+$~uD', $finding->path) !== 1) {
                throw new DriverFailure('invalid_result');
            }
            $this->integer($finding->line, 1);
            if (! in_array($finding->side, ['LEFT', 'RIGHT'], true) || ! in_array($finding->severity, ['info', 'low', 'medium', 'high', 'critical'], true)) {
                throw new DriverFailure('invalid_result');
            }
            $this->text($finding->explanation, 8192);
            $this->text($finding->evidence, 8192);
        }
        foreach ($result->tests as $test) {
            $this->keys($test, ['command', 'status', 'summary']);
            $this->text($test->command, 2048);
            $this->text($test->summary, 4096);
            if (! in_array($test->status, ['passed', 'failed', 'not_run', 'error'], true)) {
                throw new DriverFailure('invalid_result');
            }
        }

        return [
            'summary' => $result->summary,
            'outcome' => $result->outcome,
            'findings' => array_map(static fn (stdClass $finding): array => (array) $finding, $result->findings),
            'tests' => array_map(static fn (stdClass $test): array => (array) $test, $result->tests),
        ];
    }

    /**
     * @param  list<string>  $keys
     */
    private function keys(
        mixed $value,
        array $keys,
    ): void {
        if (! $value instanceof stdClass) {
            throw new DriverFailure('invalid_result');
        }
        $actual = array_keys((array) $value);
        sort($actual);
        sort($keys);
        if ($actual !== $keys) {
            throw new DriverFailure('invalid_result');
        }
    }

    private function text(
        mixed $value,
        int $max,
        string $reason = 'invalid_result',
    ): void {
        if (! is_string($value) || $value === '') {
            throw new DriverFailure($reason);
        }

        $length = preg_match_all('/./us', $value);

        if ($length === false || $length > $max) {
            throw new DriverFailure($reason);
        }
    }

    private function integer(
        mixed $value,
        int $min,
        string $reason = 'invalid_result',
    ): void {
        if (! is_int($value) || $value < $min || $value > 9_007_199_254_740_991) {
            throw new DriverFailure($reason);
        }
    }

    /**
     * @return array<string, mixed>|null
     */
    private function usage(mixed $usage): ?array
    {
        if ($usage === null) {
            return null;
        }
        if (! $usage instanceof stdClass) {
            throw new DriverFailure('malformed_output');
        }
        foreach (['input_tokens', 'output_tokens', 'cached_input_tokens'] as $field) {
            $this->integer($usage->$field ?? null, 0, 'malformed_output');
        }

        return ['input_tokens' => $usage->input_tokens, 'output_tokens' => $usage->output_tokens, 'native_limit' => null];
    }

    private function failureReason(mixed $error): string
    {
        if (! $error instanceof stdClass) {
            return 'process_error';
        }
        $code = $error->code ?? null;

        return match ($code) {
            'token_expired', 'auth_expired', 'unauthorized', 'refresh_token_expired' => 'auth_expired',
            'rate_limit_exceeded', 'usage_limit_reached' => 'rate_limited',
            'approval_required', 'approval_request' => 'approval_required',
            default => match (is_string($error->message ?? null) ? strtolower(trim($error->message)) : '') {
                'authentication expired', 'not logged in', 'unauthorized' => 'auth_expired',
                'rate limit exceeded', 'usage limit reached' => 'rate_limited',
                'approval required' => 'approval_required',
                default => 'process_error',
            },
        };
    }

    /**
     * @return array<string, mixed>
     */
    private function event(
        Claim $claim,
        int $sequence,
        string $type,
        string $message,
    ): array {
        return [
            'protocol_version' => '1.0',
            'attempt_id' => $claim->attemptId,
            'fence' => $claim->fence,
            'sequence' => $sequence,
            'type' => $type,
            'timestamp' => gmdate('Y-m-d\TH:i:s\Z'),
            'payload' => ['message' => $message],
        ];
    }
}
