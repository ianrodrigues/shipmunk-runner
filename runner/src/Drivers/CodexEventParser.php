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
     * Return no failure only after validating the complete authenticated response.
     */
    public function preflightFailureReason(
        int $exitCode,
        string $stdout,
        string $stderr = '',
    ): ?NativeFailureReason {
        $reason = null;
        $state = 'initial';
        $terminal = false;
        $completed = false;
        $message = null;
        $fatalError = false;
        $items = [];

        foreach ($this->lines($stdout, $stderr) as $line) {
            if ($terminal) {
                throw new DriverFailure(NativeFailureReason::MalformedOutput);
            }

            $event = $this->decode($line, NativeFailureReason::MalformedOutput);
            $this->assertPreflightEvent($event);

            if (str_starts_with($event->type, 'item.')) {
                $this->recordItemLifecycle($event, $items);
            }

            $type = $event->type;
            $itemFailure = str_starts_with($type, 'item.') && $event->item->type === 'error';
            $failure = $type === 'error' || $type === 'turn.failed' || $itemFailure;

            // Codex can report an error before turn.failed, but cannot resume work afterward.
            if ($reason !== null && ! $failure) {
                throw new DriverFailure(NativeFailureReason::MalformedOutput);
            }

            // Failures may precede a turn; successful work requires both start events.
            if (! $failure) {
                if ($type === 'thread.started' && $state === 'initial') {
                    $this->text($event->thread_id ?? null, 128, NativeFailureReason::MalformedOutput);
                    $state = 'thread';
                } elseif ($type === 'turn.started' && $state === 'thread') {
                    $state = 'turn';
                } elseif (
                    $state !== 'turn'
                    || (! str_starts_with($type, 'item.') && $type !== 'turn.completed')
                ) {
                    throw new DriverFailure(NativeFailureReason::MalformedOutput);
                }
            }

            if ($type === 'item.completed' && $event->item->type === 'agent_message') {
                $message = $event->item->text ?? null;
            }

            if ($type === 'turn.completed') {
                $this->assertItemsComplete($items);
                $completed = true;
            }

            $terminal = $type === 'turn.completed' || $type === 'turn.failed';
            $fatalError = $fatalError || $type === 'error';

            if ($type === 'error' || $type === 'turn.failed') {
                $reason ??= $this->failureReason($type === 'error' ? $event : ($event->error ?? null));
            }

            if ($itemFailure) {
                $reason ??= $this->failureReason($event->item);
            }
        }

        // A fatal thread error can occur before a turn exists; an item error cannot end a stream.
        if (
            $reason !== null
            && ! $terminal
            && ! $fatalError
        ) {
            throw new DriverFailure(NativeFailureReason::MalformedOutput);
        }

        if ($reason !== null) {
            return $reason;
        }

        if (
            $exitCode !== 0
            || ! $completed
            || $message !== 'SHIPMUNK_AUTH_OK'
        ) {
            return NativeFailureReason::ProcessError;
        }

        return null;
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
            throw new DriverFailure(NativeFailureReason::MalformedOutput);
        }

        if (in_array($event->type, ['item.started', 'item.updated', 'item.completed'], true)) {
            if (! ($event->item ?? null) instanceof stdClass) {
                throw new DriverFailure(NativeFailureReason::MalformedOutput);
            }

            // Preflight disables tools; only messages, reasoning and failures are valid.
            if (! in_array($event->item->type ?? null, ['agent_message', 'reasoning', 'error'], true)) {
                throw new DriverFailure(NativeFailureReason::MalformedOutput);
            }
        }
    }

    /**
     * @param array<array-key, array{type: string, complete: bool}> $items
     */
    private function recordItemLifecycle(
        stdClass $event,
        array &$items,
    ): void {
        $item = $event->item;
        $id = $this->text($item->id ?? null, 128, NativeFailureReason::MalformedOutput);
        $kind = $this->text($item->type ?? null, 128, NativeFailureReason::MalformedOutput);
        $previous = $items[$id] ?? null;

        if ($previous !== null) {
            if ($previous['complete']) {
                throw new DriverFailure(NativeFailureReason::MalformedOutput);
            }

            if ($previous['type'] !== $kind) {
                throw new DriverFailure(NativeFailureReason::MalformedOutput);
            }

            if ($event->type === 'item.started') {
                throw new DriverFailure(NativeFailureReason::MalformedOutput);
            }
        }

        // Codex emits messages, reasoning and warnings directly as completed items.
        if ($event->type === 'item.updated' && $previous === null) {
            throw new DriverFailure(NativeFailureReason::MalformedOutput);
        }

        $items[$id] = [
            'type' => $kind,
            'complete' => $event->type === 'item.completed',
        ];
    }

    /**
     * @param array<array-key, array{type: string, complete: bool}> $items
     */
    private function assertItemsComplete(array $items): void
    {
        foreach ($items as $item) {
            if (! $item['complete']) {
                throw new DriverFailure(NativeFailureReason::MalformedOutput);
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
            throw new DriverFailure(
                $exitCode === 0
                    ? NativeFailureReason::MissingResult
                    : NativeFailureReason::ProcessError,
            );
        }

        $state = 'initial';
        $items = [];
        $events = [];
        $message = null;
        $usage = null;
        $structuredMessages = 0;

        foreach ($lines as $line) {
            if ($state === 'complete') {
                throw new DriverFailure(NativeFailureReason::MalformedOutput);
            }

            $event = $this->decode($line, NativeFailureReason::MalformedOutput);
            $type = $event->type ?? null;
            if ($type === 'error' || $type === 'turn.failed') {
                throw new DriverFailure($this->failureReason($type === 'error' ? $event : ($event->error ?? null)));
            }

            if ($type === 'thread.started' && $state === 'initial') {
                $this->text($event->thread_id ?? null, 128, NativeFailureReason::MalformedOutput);
                $state = 'thread';
            } elseif ($type === 'turn.started' && $state === 'thread') {
                $state = 'turn';
                $events[] = $this->event($claim, count($events) + 1, 'progress', 'Codex execution started.');
            } elseif (in_array($type, ['item.started', 'item.updated', 'item.completed'], true) && $state === 'turn') {
                $item = $event->item ?? null;
                if (! $item instanceof stdClass) {
                    throw new DriverFailure(NativeFailureReason::MalformedOutput);
                }
                $kind = $item->type ?? null;
                if (! in_array($kind, ['agent_message', 'reasoning', 'command_execution', 'file_change', 'mcp_tool_call', 'web_search', 'todo_list', 'error'], true)) {
                    throw new DriverFailure(NativeFailureReason::MalformedOutput);
                }
                $this->recordItemLifecycle($event, $items);
                if ($kind === 'error') {
                    throw new DriverFailure($this->failureReason($item));
                }
                if ($kind === 'agent_message' && $type === 'item.completed') {
                    $this->text($item->text ?? null, self::MAX_LINE_BYTES, NativeFailureReason::InvalidResult);
                    // Earlier commentary is allowed; only the last completed message is the final result.
                    if (str_starts_with(ltrim($item->text), '{') && ++$structuredMessages > 1) {
                        throw new DriverFailure(NativeFailureReason::InvalidResult);
                    }
                    $message = $item->text;
                }
                if (in_array($kind, ['command_execution', 'file_change', 'mcp_tool_call', 'web_search'], true) && $type !== 'item.updated') {
                    $events[] = $this->event($claim, count($events) + 1, $type === 'item.started' ? 'tool_started' : 'tool_finished', 'Codex tool activity.');
                }
            } elseif ($type === 'turn.completed' && $state === 'turn') {
                $this->assertItemsComplete($items);
                $usage = $this->usage($event->usage ?? null);
                $state = 'complete';
            } else {
                throw new DriverFailure(NativeFailureReason::MalformedOutput);
            }
        }

        if ($exitCode !== 0) {
            throw new DriverFailure(NativeFailureReason::ProcessError);
        }
        if ($state !== 'complete' || $message === null) {
            throw new DriverFailure(NativeFailureReason::MissingResult);
        }

        $result = $this->result($message);
        $hasPatch = $patch !== null && $patch !== '';
        if (($hasPatch && strlen($patch) > self::MAX_PATCH_BYTES)
            || ($result['outcome'] === 'changes_proposed' && ! $hasPatch)
            || ($hasPatch && in_array($result['outcome'], ['findings', 'no_findings'], true))) {
            throw new DriverFailure(NativeFailureReason::InvalidResult);
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
            throw new DriverFailure(NativeFailureReason::MalformedOutput);
        }

        if ($stdout === '') {
            return [];
        }

        if (! str_ends_with($stdout, "\n")) {
            throw new DriverFailure(NativeFailureReason::MalformedOutput);
        }

        $lines = explode("\n", substr($stdout, 0, -1));

        if (count($lines) > self::MAX_EVENTS) {
            throw new DriverFailure(NativeFailureReason::MalformedOutput);
        }

        foreach ($lines as $line) {
            if (strlen($line) > self::MAX_LINE_BYTES) {
                throw new DriverFailure(NativeFailureReason::MalformedOutput);
            }
        }

        return $lines;
    }

    private function decode(
        string $json,
        NativeFailureReason $reason,
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
        NativeFailureReason $reason,
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
        $result = $this->decode($message, NativeFailureReason::InvalidResult);
        $this->keys($result, ['summary', 'outcome', 'findings', 'tests']);
        $this->text($result->summary, 16_384);
        if (! in_array($result->outcome, ['findings', 'no_findings', 'changes_proposed', 'incomplete', 'needs_input'], true)) {
            throw new DriverFailure(NativeFailureReason::InvalidResult);
        }
        foreach (['findings', 'tests'] as $field) {
            if (! is_array($result->$field) || count($result->$field) > 100) {
                throw new DriverFailure(NativeFailureReason::InvalidResult);
            }
        }
        if (($result->outcome === 'findings' && $result->findings === []) || ($result->outcome === 'no_findings' && $result->findings !== [])) {
            throw new DriverFailure(NativeFailureReason::InvalidResult);
        }
        foreach ($result->findings as $finding) {
            $this->keys($finding, ['path', 'line', 'side', 'severity', 'explanation', 'evidence']);
            $this->text($finding->path, 1024);
            if (preg_match('~^(?!/)(?!.*(?:^|/)\.\.(?:/|$))(?!.*\\\\)[^\x00-\x1f]+$~uD', $finding->path) !== 1) {
                throw new DriverFailure(NativeFailureReason::InvalidResult);
            }
            $this->integer($finding->line, 1);
            if (! in_array($finding->side, ['LEFT', 'RIGHT'], true) || ! in_array($finding->severity, ['info', 'low', 'medium', 'high', 'critical'], true)) {
                throw new DriverFailure(NativeFailureReason::InvalidResult);
            }
            $this->text($finding->explanation, 8192);
            $this->text($finding->evidence, 8192);
        }
        foreach ($result->tests as $test) {
            $this->keys($test, ['command', 'status', 'summary']);
            $this->text($test->command, 2048);
            $this->text($test->summary, 4096);
            if (! in_array($test->status, ['passed', 'failed', 'not_run', 'error'], true)) {
                throw new DriverFailure(NativeFailureReason::InvalidResult);
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
            throw new DriverFailure(NativeFailureReason::InvalidResult);
        }
        $actual = array_keys((array) $value);
        sort($actual);
        sort($keys);
        if ($actual !== $keys) {
            throw new DriverFailure(NativeFailureReason::InvalidResult);
        }
    }

    private function text(
        mixed $value,
        int $max,
        NativeFailureReason $reason = NativeFailureReason::InvalidResult,
    ): string {
        if (! is_string($value) || $value === '') {
            throw new DriverFailure($reason);
        }

        $length = preg_match_all('/./us', $value);

        if ($length === false || $length > $max) {
            throw new DriverFailure($reason);
        }

        return $value;
    }

    private function integer(
        mixed $value,
        int $min,
        NativeFailureReason $reason = NativeFailureReason::InvalidResult,
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
            throw new DriverFailure(NativeFailureReason::MalformedOutput);
        }
        foreach (['input_tokens', 'output_tokens', 'cached_input_tokens'] as $field) {
            $this->integer($usage->$field ?? null, 0, NativeFailureReason::MalformedOutput);
        }

        return ['input_tokens' => $usage->input_tokens, 'output_tokens' => $usage->output_tokens, 'native_limit' => null];
    }

    private function failureReason(mixed $error): NativeFailureReason
    {
        if (! $error instanceof stdClass) {
            return NativeFailureReason::ProcessError;
        }

        $reason = $this->failureCode($error->code ?? null);

        if ($reason !== null) {
            return $reason;
        }

        $message = is_string($error->message ?? null) ? trim($error->message) : '';

        // The pinned CLI places a backend JSON error body inside its message field.
        if (str_starts_with($message, '{')) {
            try {
                $body = $this->decode($message, NativeFailureReason::ProcessError);
            } catch (DriverFailure) {
                return NativeFailureReason::ProcessError;
            }

            if (! ($body->error ?? null) instanceof stdClass) {
                return NativeFailureReason::ProcessError;
            }

            return $this->failureCode($body->error->code ?? null)
                ?? NativeFailureReason::ProcessError;
        }

        return match (strtolower($message)) {
            'authentication expired', 'not logged in', 'unauthorized' => NativeFailureReason::AuthExpired,
            'rate limit exceeded', 'usage limit reached' => NativeFailureReason::RateLimited,
            'approval required' => NativeFailureReason::ApprovalRequired,
            default => NativeFailureReason::ProcessError,
        };
    }

    private function failureCode(mixed $code): ?NativeFailureReason
    {
        return match ($code) {
            'token_expired', 'auth_expired', 'unauthorized', 'refresh_token_expired' => NativeFailureReason::AuthExpired,
            'rate_limit_exceeded', 'usage_limit_reached' => NativeFailureReason::RateLimited,
            'approval_required', 'approval_request' => NativeFailureReason::ApprovalRequired,
            'model_not_found' => NativeFailureReason::ModelUnavailable,
            'invalid_json_schema' => NativeFailureReason::InvalidOutputSchema,
            default => null,
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
