<?php

declare(strict_types=1);

use Shipmunk\Runner\Claim;
use Shipmunk\Runner\Drivers\CodexEventParser;
use Shipmunk\Runner\Drivers\DriverFailure;

require dirname(__DIR__).'/bootstrap.php';

function parser_assert(bool $condition, string $message): void
{
    if (! $condition) {
        throw new RuntimeException($message);
    }
}

function parser_claim(): Claim
{
    return new Claim(
        '01k4w000000000000000000001',
        '01k4w000000000000000000002',
        3,
        new DateTimeImmutable('@1045'),
        new DateTimeImmutable('@1900'),
        ['protocol_version' => '1.0'],
    );
}

function parser_stream(array|string $result, mixed $usage = null): string
{
    return implode("\n", array_map(static fn (array $event): string => json_encode($event, JSON_THROW_ON_ERROR), [
        ['type' => 'thread.started', 'thread_id' => 'synthetic-thread'],
        ['type' => 'turn.started'],
        ['type' => 'item.completed', 'item' => [
            'id' => 'item_1',
            'type' => 'agent_message',
            'text' => is_string($result) ? $result : json_encode($result, JSON_THROW_ON_ERROR),
        ]],
        ['type' => 'turn.completed', 'usage' => $usage],
    ]))."\n";
}

function parser_rejects(string $name, string $reason, string $stdout, int $exit = 0, string $stderr = '', ?string $patch = null): void
{
    try {
        (new CodexEventParser)->parse(parser_claim(), $exit, $stdout, $stderr, $patch);
    } catch (DriverFailure $failure) {
        parser_assert($failure->reason->value === $reason, $name.': incorrect reason '.$failure->reason->value);
        parser_assert(! str_contains((string) $failure, 'SYNTHETIC_SECRET'), $name.': provider text leaked');
        fwrite(STDOUT, "PASS {$name}\n");

        return;
    }

    throw new RuntimeException($name.': accepted invalid native output');
}

$parser = new CodexEventParser;
$fixture = file_get_contents(__DIR__.'/fixtures/codex/success.jsonl');
$execution = $parser->parse(parser_claim(), 0, $fixture, 'SYNTHETIC_SECRET');
parser_assert($execution->result === [
    'protocol_version' => '1.0',
    'run_id' => '01k4w000000000000000000001',
    'attempt_id' => '01k4w000000000000000000002',
    'fence' => 3,
    'summary' => 'Review complete.',
    'outcome' => 'no_findings',
    'findings' => [],
    'tests' => [['command' => 'php tests.php', 'status' => 'passed', 'summary' => 'All checks passed.']],
    'patch_artifact' => null,
    'usage' => ['input_tokens' => 24763, 'output_tokens' => 122, 'native_limit' => null],
], 'Normalized result differs from contract.');
parser_assert($execution->artifacts === [], 'Invented native artifacts.');
parser_assert(array_column($execution->events, 'type') === ['progress', 'tool_started', 'tool_finished'], 'Native events were not normalized.');
parser_assert(! str_contains(json_encode($execution), 'SYNTHETIC_SECRET'), 'Raw native text escaped normalization.');
foreach ($execution->events as $index => $event) {
    parser_assert($event['attempt_id'] === parser_claim()->attemptId && $event['fence'] === 3 && $event['sequence'] === $index + 1, 'Event authority must come from the claim.');
}
fwrite(STDOUT, "PASS normalizes synthetic success and suppresses raw provider diagnostics\n");

$result = ['summary' => 'Done.', 'outcome' => 'no_findings', 'findings' => [], 'tests' => []];
$valid = parser_stream($result);
parser_assert($parser->parse(parser_claim(), 0, $valid)->result['usage'] === null, 'Invented unknown usage.');
$malformed = [
    'truncated final line' => substr($valid, 0, -1),
    'invalid JSON' => "{bad}\n",
    'scalar event' => "42\n",
    'empty event' => "\n",
    'duplicate JSON key' => "{\"type\":\"thread.started\",\"type\":\"turn.started\"}\n",
    'escaped duplicate JSON key' => "{\"type\":\"thread.started\",\"t\\u0079pe\":\"turn.started\"}\n",
    'unknown event' => str_replace('turn.started', 'turn.unknown', $valid),
    'unknown item type' => str_replace('agent_message', 'untrusted_tool', $valid),
    'duplicate thread' => explode("\n", $valid)[0]."\n".$valid,
    'turn before thread' => "{\"type\":\"turn.started\"}\n".$valid,
    'duplicate terminal' => $valid."{\"type\":\"turn.completed\"}\n",
    'after terminal' => $valid."{\"type\":\"turn.started\"}\n",
    'line bound' => str_repeat(' ', CodexEventParser::MAX_LINE_BYTES + 1)."\n",
    'output bound' => str_repeat(' ', CodexEventParser::MAX_OUTPUT_BYTES + 1)."\n",
    'event bound' => str_repeat("{}\n", CodexEventParser::MAX_EVENTS + 1),
    'unfinished item' => str_replace('item.completed', 'item.started', $valid),
    'update without start' => str_replace('item.completed', 'item.updated', $valid),
    'invalid usage' => parser_stream($result, ['input_tokens' => -1, 'output_tokens' => 1, 'cached_input_tokens' => 0]),
];
foreach ($malformed as $name => $output) {
    parser_rejects($name, 'malformed_output', $output);
}
$streamLines = explode("\n", $valid);
parser_rejects('duplicate completed item', 'malformed_output', implode("\n", [...array_slice($streamLines, 0, 3), $streamLines[2], ...array_slice($streamLines, 3)]));
$secondMessage = str_replace('item_1', 'item_2', $streamLines[2]);
parser_rejects('duplicate structured result', 'invalid_result', implode("\n", [...array_slice($streamLines, 0, 3), $secondMessage, ...array_slice($streamLines, 3)]));
parser_rejects('item type changes midstream', 'malformed_output', str_replace('"id":"item_1","type":"command_execution","command":"echo SYNTHETIC_SECRET","aggregated_output"', '"id":"item_1","type":"file_change","command":"echo SYNTHETIC_SECRET","aggregated_output"', $fixture));
parser_rejects('stderr bound', 'malformed_output', $valid, 0, str_repeat('x', CodexEventParser::MAX_OUTPUT_BYTES));
parser_rejects('missing terminal', 'missing_result', implode("\n", array_slice(explode("\n", $valid), 0, 3))."\n");
parser_rejects('missing message', 'missing_result', "{\"type\":\"thread.started\",\"thread_id\":\"synthetic\"}\n{\"type\":\"turn.started\"}\n{\"type\":\"turn.completed\"}\n");
parser_rejects('empty successful stream', 'missing_result', '');
parser_rejects('nonzero process', 'process_error', $valid, 1, 'SYNTHETIC_SECRET');
parser_rejects('empty failed process', 'process_error', '', 1, 'SYNTHETIC_SECRET');

$invalid = [
    'authority injection' => [...$result, 'fence' => 999],
    'patch injection' => [...$result, 'patch_artifact' => []],
    'cost injection' => [...$result, 'cost' => 1],
    'empty summary' => [...$result, 'summary' => ''],
    'long summary' => [...$result, 'summary' => str_repeat('a', 16385)],
    'unknown outcome' => [...$result, 'outcome' => 'success'],
    'findings required' => [...$result, 'outcome' => 'findings'],
    'object findings' => [...$result, 'findings' => (object) []],
    'too many findings' => [...$result, 'findings' => array_fill(0, 101, [])],
    'object tests' => [...$result, 'tests' => (object) []],
    'too many tests' => [...$result, 'tests' => array_fill(0, 101, [])],
    'missing test fields' => [...$result, 'tests' => [[]]],
    'duplicate summary' => '{"summary":"first","summary":"last","outcome":"no_findings","findings":[],"tests":[]}',
    'nonstructured message' => 'SYNTHETIC_SECRET',
];
$finding = ['path' => 'src/file.php', 'line' => 1, 'side' => 'RIGHT', 'severity' => 'high', 'explanation' => 'Defect.', 'evidence' => 'Evidence.'];
$findingsResult = [...$result, 'outcome' => 'findings', 'findings' => [$finding]];
parser_assert($parser->parse(parser_claim(), 0, parser_stream($findingsResult))->result['findings'] === [$finding], 'Valid finding was changed.');
foreach ([
    'path' => ['', '/absolute', '../outside', 'src/../outside', 'src\\outside', "src/\x00file", str_repeat('a', 1025)],
    'line' => [0, -1, 1.5, '1', 9007199254740992],
    'side' => ['right', null],
    'severity' => ['urgent', null],
    'explanation' => ['', str_repeat('a', 8193)],
    'evidence' => ['', str_repeat('a', 8193)],
] as $field => $values) {
    foreach ($values as $index => $value) {
        $invalid['finding '.$field.' '.$index] = [...$findingsResult, 'findings' => [[...$finding, $field => $value]]];
    }
}
$invalid['unexpected finding field'] = [...$findingsResult, 'findings' => [[...$finding, 'secret' => 'SYNTHETIC_SECRET']]];
$invalid['no_findings with finding'] = [...$result, 'findings' => [$finding]];
$test = ['command' => 'php tests.php', 'status' => 'passed', 'summary' => 'Passed.'];
foreach (['command' => ['', str_repeat('x', 2049)], 'status' => ['success', null], 'summary' => ['', str_repeat('x', 4097)]] as $field => $values) {
    foreach ($values as $index => $value) {
        $invalid['test '.$field.' '.$index] = [...$result, 'tests' => [[...$test, $field => $value]]];
    }
}
$invalid['unexpected test field'] = [...$result, 'tests' => [[...$test, 'extra' => true]]];
foreach ($invalid as $name => $value) {
    parser_rejects($name, 'invalid_result', parser_stream($value));
}

foreach (['auth_expired' => 'token_expired', 'rate_limited' => 'rate_limit_exceeded', 'approval_required' => 'approval_required', 'process_error' => 'unknown'] as $reason => $code) {
    foreach (['error', 'turn.failed'] as $type) {
        $error = ['code' => $code, 'message' => 'SYNTHETIC_SECRET'];
        $event = $type === 'error' ? ['type' => $type, ...$error] : ['type' => $type, 'error' => $error];
        parser_rejects($type.' '.$reason, $reason, json_encode($event)."\n", 1);
    }
}
foreach (['authentication expired' => 'auth_expired', 'rate limit exceeded' => 'rate_limited', 'approval required' => 'approval_required', 'SYNTHETIC_SECRET rate limit exceeded' => 'process_error'] as $message => $reason) {
    parser_rejects('safe error matching '.$reason, $reason, json_encode(['type' => 'error', 'message' => $message])."\n", 1);
}

foreach ([
    'model_not_found' => 'model_unavailable',
    'invalid_json_schema' => 'invalid_output_schema',
    'token_expired' => 'auth_expired',
    'rate_limit_exceeded' => 'rate_limited',
    'approval_required' => 'approval_required',
    'unknown' => 'process_error',
] as $code => $reason) {
    foreach (['error', 'turn.failed'] as $type) {
        $error = ['code' => $code, 'message' => 'SYNTHETIC_SECRET'];
        $message = json_encode(['error' => $error], JSON_THROW_ON_ERROR);
        $event = $type === 'error'
            ? ['type' => $type, 'message' => $message]
            : ['type' => $type, 'error' => ['message' => $message]];

        parser_rejects(
            'nested backend '.$type.' '.$reason,
            $reason,
            json_encode($event, JSON_THROW_ON_ERROR)."\n",
            1,
        );
    }
}

foreach ([
    'truncated JSON' => '{"error":{"code":"model_not_found"',
    'duplicate code' => '{"error":{"code":"model_not_found","code":"invalid_json_schema"}}',
    'escaped duplicate code' => '{"error":{"code":"model_not_found","c\\u006fde":"invalid_json_schema"}}',
    'duplicate envelope' => '{"error":{"code":"model_not_found"},"error":{}}',
    'array body' => '[{"error":{"code":"model_not_found"}}]',
    'array error' => '{"error":[{"code":"model_not_found"}]}',
    'scalar error' => '{"error":"model_not_found"}',
    'array code' => '{"error":{"code":["model_not_found"]}}',
    'missing envelope' => '{"code":"model_not_found"}',
    'recursive envelope' => '{"error":{"error":{"code":"model_not_found"}}}',
    'unknown request code' => '{"error":{"type":"invalid_request_error","code":"new_backend_error"}}',
    'nested message' => '{"error":{"message":"model_not_found"}}',
    'unstructured mention' => 'SYNTHETIC_SECRET model_not_found invalid_json_schema',
    'JSON depth bound' => str_repeat('{"error":', 33).'{}'.str_repeat('}', 33),
] as $name => $message) {
    parser_rejects(
        'backend diagnostic rejects '.$name,
        'process_error',
        json_encode(['type' => 'error', 'message' => $message], JSON_THROW_ON_ERROR)."\n",
        1,
    );
}

parser_rejects(
    'known native code takes precedence over malformed backend message',
    'auth_expired',
    json_encode([
        'type' => 'error',
        'code' => 'token_expired',
        'message' => '{SYNTHETIC_SECRET',
    ], JSON_THROW_ON_ERROR)."\n",
    1,
);

$changes = parser_stream([...$result, 'outcome' => 'changes_proposed']);
parser_rejects('changes require trusted patch', 'invalid_result', $changes);
parser_rejects('review cannot emit patch', 'invalid_result', $valid, patch: 'diff');
parser_rejects('patch bound', 'invalid_result', $changes, patch: str_repeat('x', CodexEventParser::MAX_PATCH_BYTES + 1));
$patched = $parser->parse(parser_claim(), 0, $changes, patch: 'trusted diff');
parser_assert($patched->artifacts === [['kind' => 'patch', 'bytes' => 'trusted diff', 'sha256' => hash('sha256', 'trusted diff')]], 'Trusted patch was not preserved.');
parser_assert($patched->result['patch_artifact'] === ['artifact_id' => parser_claim()->attemptId, 'sha256' => hash('sha256', 'trusted diff')], 'Trusted patch reference was not derived.');
foreach (['incomplete', 'needs_input'] as $outcome) {
    $incomplete = $parser->parse(parser_claim(), 0, parser_stream([...$result, 'outcome' => $outcome]), patch: 'discarded diff');
    parser_assert($incomplete->artifacts === [] && $incomplete->result['patch_artifact'] === null, 'Incomplete result published a patch.');
}
fwrite(STDOUT, "PASS trusted patch binding and incomplete artifact suppression\n");
