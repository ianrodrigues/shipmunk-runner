<?php

declare(strict_types=1);

use Shipmunk\Runner\Claim;
use Shipmunk\Runner\CommandResult;
use Shipmunk\Runner\Drivers\AgentDriver;
use Shipmunk\Runner\Drivers\AgentSession;
use Shipmunk\Runner\Drivers\AgentSessionStore;
use Shipmunk\Runner\Drivers\AgentTransport;
use Shipmunk\Runner\Drivers\CodexDriver;
use Shipmunk\Runner\Drivers\DriverFailure;
use Shipmunk\Runner\Drivers\TrustedAgentInstructions;

require dirname(__DIR__).'/bootstrap.php';

final class SimulatedAgentTransport implements AgentTransport
{
    public array $commands = [];

    public string $version = 'codex-cli 0.154.0';

    public string $mode = 'Logged in using ChatGPT';

    public string $stdout;

    public int $exitCode = 0;

    public bool $stopped = false;

    public ?string $diff = null;

    public ?CommandResult $preflightResult = null;

    public function __construct()
    {
        $this->stdout = file_get_contents(__DIR__.'/fixtures/codex/success.jsonl');
    }

    public function run(
        array $argv,
        string $stdin,
        Closure $checkpoint,
    ): CommandResult {
        $checkpoint(0);
        $this->commands[] = ['argv' => $argv, 'stdin' => $stdin];

        if (in_array('--version', $argv, true)) {
            return new CommandResult(0, $this->version, '');
        }

        if (in_array('status', $argv, true)) {
            return new CommandResult(0, '', $this->mode);
        }

        if (in_array('--ephemeral', $argv, true)) {
            return $this->preflightResult ?? new CommandResult(
                0,
                file_get_contents(__DIR__.'/fixtures/codex/preflight-success.jsonl'),
                '',
            );
        }

        return new CommandResult($this->exitCode, $this->stdout, 'SYNTHETIC_SECRET');
    }

    public function patch(): ?string
    {
        return $this->diff;
    }

    public function changedFiles(): array
    {
        return [];
    }

    public function stop(): void
    {
        $this->stopped = true;
    }
}

function driver_claim(array $overrides = []): Claim
{
    return new Claim(
        '01kkkkkkkkkkkkkkkkkkkkkkkk',
        '01mmmmmmmmmmmmmmmmmmmmmmmm',
        4,
        new DateTimeImmutable('2026-09-12T12:00:45Z'),
        new DateTimeImmutable('2026-09-12T12:15:00Z'),
        array_replace_recursive([
            'protocol_version' => '1.0',
            'agent' => 'codex',
            'runtime_version' => '0.154.0',
            'profile_id' => '01nnnnnnnnnnnnnnnnnnnnnnnn',
            'repository_id' => 12,
            'head_sha' => str_repeat('a', 40),
            'base_sha' => str_repeat('b', 40),
            'supervisor' => ['credential_reference' => 'credential:assigned'],
            'effective_config' => [
                'model' => 'fixture-model',
                'instructions' => 'Approved dashboard instructions.',
                'instructions_sha256' => hash('sha256', 'Approved dashboard instructions.'),
                'trusted_revision' => str_repeat('c', 40),
                'effective_configuration_sha256' => str_repeat('d', 64),
            ],
            'instruction_artifacts' => [['artifact_id' => '01pppppppppppppppppppppppp', 'sha256' => str_repeat('e', 64)]],
            'task_context' => 'Review the supplied repository.',
        ], $overrides),
    );
}

function driver_assert(bool $value): void
{
    if (! $value) {
        throw new RuntimeException('Driver contract assertion failed.');
    }
}

function driver_rejects(
    Closure $operation,
    ?string $reason = null,
    ?string $stage = null,
    ?int $exitCode = null,
): void {
    try {
        $operation();
    } catch (RuntimeException $exception) {
        if ($reason !== null) {
            driver_assert($exception instanceof DriverFailure && $exception->reason->value === $reason);
        }

        if ($stage !== null) {
            driver_assert($exception instanceof DriverFailure);
            driver_assert($exception->stage?->value === $stage);
            driver_assert($exception->exitCode === $exitCode);
            driver_assert(! str_contains($exception->summary(), 'SYNTHETIC_SECRET'));
        }

        return;
    }

    throw new RuntimeException('Expected driver rejection.');
}

$tests = [];
$tests['shared lifecycle inspects authenticates executes and stops with isolated transport'] = function (): void {
    $transport = new SimulatedAgentTransport;
    $driver = new CodexDriver('Approved AGENTS instructions.');
    $claim = driver_claim();
    $checkpoints = 0;
    $checkpoint = function (int $minimumSeconds) use (&$checkpoints): void {
        driver_assert($minimumSeconds >= 0);
        $checkpoints++;
    };

    driver_assert($driver instanceof AgentDriver);
    driver_assert($driver->inspect($transport, $checkpoint)['version'] === '0.154.0');
    $driver->probe($transport, $checkpoint);
    $result = $driver->start($claim, $transport, $checkpoint);
    $driver->stop($transport);

    driver_assert($result->result['fence'] === 4 && $result->result['outcome'] === 'no_findings');
    driver_assert($transport->stopped && $checkpoints >= 7);
    $command = $transport->commands[4];
    driver_assert($command['stdin'] === $claim->manifest['task_context']);
    driver_assert(in_array('--ignore-user-config', $command['argv'], true));
    driver_assert(in_array('--ignore-rules', $command['argv'], true));
    driver_assert(in_array('approval_policy="never"', $command['argv'], true));
    driver_assert(! in_array('--dangerously-bypass-approvals-and-sandbox', $command['argv'], true));
    driver_assert(! in_array('--sandbox', $command['argv'], true));
    driver_assert(! str_contains(json_encode($result), 'SYNTHETIC_SECRET'));
    driver_assert(str_contains(implode("\n", $command['argv']), 'Approved AGENTS instructions.'));
};
$tests['runtime version mismatch prevents execution'] = function (): void {
    $transport = new SimulatedAgentTransport;
    $transport->version = 'codex-cli 0.155.0';
    driver_rejects(fn () => (new CodexDriver)->inspect($transport, static function (): void {}), 'process_error');
    driver_assert(count($transport->commands) === 1);
};
$tests['API account mode cannot pass native subscription preflight'] = function (): void {
    $transport = new SimulatedAgentTransport;
    $transport->mode = 'Logged in using an API key';
    driver_rejects(fn () => (new CodexDriver)->probe($transport, static function (): void {}), 'auth_expired');
    driver_assert(count($transport->commands) === 1);
};
$tests['version mismatch diagnostics retain the successful process exit and safe stage'] = function (): void {
    $transport = new SimulatedAgentTransport;
    $transport->version = 'SYNTHETIC_SECRET';

    driver_rejects(
        fn () => (new CodexDriver)->inspect($transport, static function (): void {}),
        'process_error',
        'version_inspection',
        0,
    );
};
$tests['account mismatch diagnostics identify the probe without exposing account output'] = function (): void {
    $transport = new SimulatedAgentTransport;
    $transport->mode = 'SYNTHETIC_SECRET';

    driver_rejects(
        fn () => (new CodexDriver)->probe($transport, static function (): void {}),
        'auth_expired',
        'account_probe',
        0,
    );
};
$tests['preflight diagnostics retain the native failure exit code'] = function (): void {
    $transport = new SimulatedAgentTransport;
    $transport->preflightResult = new CommandResult(17, '', 'SYNTHETIC_SECRET');

    driver_rejects(
        fn () => (new CodexDriver)->probe($transport, static function (): void {}),
        'process_error',
        'preflight',
        17,
    );
};
$tests['execution diagnostics preserve failure stage before any later account probe'] = function (): void {
    $transport = new SimulatedAgentTransport;
    $transport->exitCode = 23;
    $transport->stdout = '';

    driver_rejects(
        fn () => (new CodexDriver)->start(driver_claim(), $transport, static function (): void {}),
        'process_error',
        'execution',
        23,
    );
    driver_assert(count($transport->commands) === 1);
};
$tests['successful processes with malformed output identify result validation'] = function (): void {
    $transport = new SimulatedAgentTransport;
    $transport->stdout = "SYNTHETIC_SECRET\n";

    driver_rejects(
        fn () => (new CodexDriver)->start(driver_claim(), $transport, static function (): void {}),
        'malformed_output',
        'result_parsing',
        0,
    );
};
$tests['backend model rejection preserves safe execution diagnostics'] = function (): void {
    $transport = new SimulatedAgentTransport;
    $transport->exitCode = 1;
    $transport->stdout = json_encode([
        'type' => 'error',
        'message' => json_encode([
            'error' => [
                'code' => 'model_not_found',
                'message' => 'SYNTHETIC_SECRET',
            ],
        ], JSON_THROW_ON_ERROR),
    ], JSON_THROW_ON_ERROR)."\n";

    driver_rejects(
        fn () => (new CodexDriver)->start(driver_claim(), $transport, static function (): void {}),
        'model_unavailable',
        'execution',
        1,
    );
    driver_assert(count($transport->commands) === 1);
};
$tests['preflight keeps the first backend rejection after full stream validation'] = function (): void {
    $message = json_encode([
        'error' => [
            'code' => 'model_not_found',
            'message' => 'SYNTHETIC_SECRET',
        ],
    ], JSON_THROW_ON_ERROR);
    $stdout = json_encode(['type' => 'error', 'message' => $message], JSON_THROW_ON_ERROR)."\n";
    $stdout .= "{\"type\":\"turn.failed\",\"error\":{\"message\":\"{malformed\"}}\n";
    $transport = new SimulatedAgentTransport;
    $transport->preflightResult = new CommandResult(1, $stdout, 'SYNTHETIC_SECRET');

    driver_rejects(
        fn () => (new CodexDriver)->probe($transport, static function (): void {}),
        'model_unavailable',
        'preflight',
        1,
    );
};
foreach ([
    'rate_limit_exceeded' => 'rate_limited',
    'token_expired' => 'auth_expired',
    'approval_required' => 'approval_required',
    'unknown' => 'process_error',
] as $code => $reason) {
    foreach (['error', 'turn.failed', 'item.completed'] as $type) {
        $tests['preflight '.$type.' preserves '.$reason] = function () use ($code, $reason, $type): void {
            $transport = new SimulatedAgentTransport;
            $error = [
                'code' => $code,
                'message' => 'SYNTHETIC_SECRET',
            ];
            $event = match ($type) {
                'error' => ['type' => $type, ...$error],
                'turn.failed' => ['type' => $type, 'error' => $error],
                'item.completed' => ['type' => $type, 'item' => ['id' => 'error-item', 'type' => 'error', ...$error]],
            };
            $stdout = json_encode($event)."\n";

            if ($type !== 'turn.failed') {
                $stdout .= "{\"type\":\"error\",\"code\":\"unknown\"}\n";
            }

            $transport->preflightResult = new CommandResult(
                1,
                $stdout,
                'SYNTHETIC_SECRET',
            );

            driver_rejects(fn () => (new CodexDriver)->probe($transport, static function (): void {}), $reason);

            driver_assert(count($transport->commands) === 2);
        };
    }
}
foreach (['error', 'item.completed'] as $type) {
    foreach ([
        'invalid JSON' => "not-json\n",
        'unknown event' => "{\"type\":\"future.failure\",\"message\":\"SYNTHETIC_SECRET\"}\n",
        'missing event type' => "{}\n",
        'unknown item' => "{\"type\":\"item.completed\",\"item\":{\"type\":\"future_tool\"}}\n",
        'missing item' => "{\"type\":\"item.completed\"}\n",
        'tool item' => "{\"type\":\"item.completed\",\"item\":{\"type\":\"command_execution\"}}\n",
        'successful turn' => "{\"type\":\"turn.completed\"}\n",
        'fresh thread' => "{\"type\":\"thread.started\",\"thread_id\":\"synthetic-thread\"}\n",
        'fresh message' => "{\"type\":\"item.completed\",\"item\":{\"id\":\"auth-message\",\"type\":\"agent_message\",\"text\":\"SHIPMUNK_AUTH_OK\"}}\n",
        'duplicate keys' => "{\"type\":\"error\",\"code\":\"token_expired\",\"code\":\"rate_limit_exceeded\"}\n",
    ] as $name => $trailing) {
        $tests['preflight '.$type.' cannot hide trailing '.$name] = function () use ($type, $trailing): void {
            $transport = new SimulatedAgentTransport;
            $error = [
                'type' => 'error',
                'code' => 'rate_limit_exceeded',
            ];
            $event = $type === 'error' ? $error : [
                'type' => $type,
                'item' => ['id' => 'error-item', ...$error],
            ];
            $transport->preflightResult = new CommandResult(1, json_encode($event)."\n".$trailing, '');

            driver_rejects(fn () => (new CodexDriver)->probe($transport, static function (): void {}), 'malformed_output');

            driver_assert(count($transport->commands) === 2);
        };
    }
}
foreach ([
    'orphaned update' => ['item.updated'],
    'duplicate start' => ['item.started', 'item.started'],
    'update after completion' => ['item.completed', 'item.updated'],
    'duplicate completion' => ['item.completed', 'item.completed'],
] as $name => $types) {
    $tests['preflight rejects '.$name.' before retaining a rate limit'] = function () use ($types): void {
        $transport = new SimulatedAgentTransport;
        $stdout = '';

        foreach ($types as $type) {
            $stdout .= json_encode([
                'type' => $type,
                'item' => [
                    'id' => 'error-item',
                    'type' => 'error',
                    'code' => 'rate_limit_exceeded',
                ],
            ])."\n";
        }

        $transport->preflightResult = new CommandResult(1, $stdout, '');

        driver_rejects(fn () => (new CodexDriver)->probe($transport, static function (): void {}), 'malformed_output');
    };
}
$tests['preflight preserves a rate limit through a valid item lifecycle'] = function (): void {
    $transport = new SimulatedAgentTransport;
    $stdout = '';

    foreach (['item.started', 'item.updated', 'item.completed'] as $type) {
        $stdout .= json_encode([
            'type' => $type,
            'item' => [
                'id' => 'error-item',
                'type' => 'error',
                'code' => 'rate_limit_exceeded',
            ],
        ])."\n";
    }

    $stdout .= json_encode(['type' => 'turn.failed'])."\n";
    $transport->preflightResult = new CommandResult(1, $stdout, '');

    driver_rejects(fn () => (new CodexDriver)->probe($transport, static function (): void {}), 'rate_limited');
};
$tests['preflight permits an error followed by its failed turn'] = function (): void {
    $transport = new SimulatedAgentTransport;
    $transport->preflightResult = new CommandResult(
        1,
        "{\"type\":\"error\",\"code\":\"rate_limit_exceeded\"}\n"
            ."{\"type\":\"turn.failed\",\"error\":{\"code\":\"unknown\"}}\n",
        '',
    );

    driver_rejects(fn () => (new CodexDriver)->probe($transport, static function (): void {}), 'rate_limited');
};
foreach (['turn.completed', 'turn.failed'] as $type) {
    $tests['preflight rejects events after terminal '.$type] = function () use ($type): void {
        $transport = new SimulatedAgentTransport;
        $terminal = $type === 'turn.completed'
            ? file_get_contents(__DIR__.'/fixtures/codex/preflight-success.jsonl')
            : json_encode(['type' => $type])."\n";
        $transport->preflightResult = new CommandResult(
            1,
            $terminal."{\"type\":\"error\",\"code\":\"rate_limit_exceeded\"}\n",
            '',
        );

        driver_rejects(fn () => (new CodexDriver)->probe($transport, static function (): void {}), 'malformed_output');
    };
}
$preflightSuccess = file_get_contents(__DIR__.'/fixtures/codex/preflight-success.jsonl');
$preflightLines = explode("\n", $preflightSuccess);
$preflightCompletion = implode("\n", array_slice($preflightLines, 2));

foreach ([
    'empty process error' => ['', 'SYNTHETIC_SECRET', 1, 'process_error'],
    'nonzero successful response' => [$preflightSuccess, '', 1, 'process_error'],
    'missing start events' => [$preflightCompletion, '', 0, 'malformed_output'],
    'missing thread start' => [
        implode("\n", array_slice($preflightLines, 1)),
        '',
        0,
        'malformed_output',
    ],
    'missing turn start' => [
        $preflightLines[0]."\n".$preflightCompletion,
        '',
        0,
        'malformed_output',
    ],
    'reordered start events' => [
        $preflightLines[1]."\n".$preflightLines[0]."\n".$preflightCompletion,
        '',
        0,
        'malformed_output',
    ],
    'duplicate thread start' => [
        $preflightLines[0]."\n".$preflightSuccess,
        '',
        0,
        'malformed_output',
    ],
    'missing thread identity' => [
        str_replace(',"thread_id":"synthetic-preflight-thread"', '', $preflightSuccess),
        '',
        0,
        'malformed_output',
    ],
    'unfinished item with a successful auth message' => [
        file_get_contents(__DIR__.'/fixtures/codex/preflight-unfinished-item.jsonl'),
        '',
        0,
        'malformed_output',
    ],
    'unrecognized successful response' => ["{\"type\":\"turn.completed\"}\n", '', 0, 'malformed_output'],
    'truncated output' => ['{"type":"error","code":"token_expired"}', '', 1, 'malformed_output'],
    'duplicate error code' => ["{\"type\":\"error\",\"code\":\"token_expired\",\"code\":\"rate_limit_exceeded\"}\n", '', 1, 'malformed_output'],
    'oversized output' => ['', str_repeat('x', 2_097_153), 1, 'malformed_output'],
    'oversized line' => [str_repeat(' ', 65_537)."\n", '', 1, 'malformed_output'],
    'too many events' => [str_repeat("{}\n", 10_001), '', 1, 'malformed_output'],
] as $name => [$stdout, $stderr, $exitCode, $reason]) {
    $tests['preflight rejects '.$name.' without inventing expired authentication'] = function () use ($stdout, $stderr, $exitCode, $reason): void {
        $transport = new SimulatedAgentTransport;
        $transport->preflightResult = new CommandResult($exitCode, $stdout, $stderr);

        driver_rejects(fn () => (new CodexDriver)->probe($transport, static function (): void {}), $reason);

        driver_assert(count($transport->commands) === 2);
    };
}
$tests['native failure remains distinct before any post-exit account command'] = function (): void {
    $transport = new SimulatedAgentTransport;
    $transport->exitCode = 1;
    $transport->stdout = "{\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"synthetic\"}\n";
    driver_rejects(fn () => (new CodexDriver)->start(driver_claim(), $transport, static function (): void {}), 'rate_limited');
    driver_assert(count($transport->commands) === 1);
};
$tests['explicit compatible session resumes its UUID'] = function (): void {
    $transport = new SimulatedAgentTransport;
    $claim = driver_claim();
    $id = json_decode(strtok($transport->stdout, "\n"), true)['thread_id'];
    $session = new AgentSession($id, AgentSession::binding($claim));

    (new CodexDriver)->start($claim, $transport, static function (): void {}, $session);

    $argv = $transport->commands[0]['argv'];
    driver_assert(array_slice($argv, -3) === ['resume', $id, '-']);
    driver_assert(! in_array('--last', $argv, true));
    driver_assert(str_contains($transport->commands[0]['stdin'], 'Previous edits and command side effects are absent.'));
};
foreach ([
    ['profile_id' => '01qqqqqqqqqqqqqqqqqqqqqqqq'],
    ['supervisor' => ['credential_reference' => 'credential:rotated']],
    ['head_sha' => str_repeat('f', 40)],
    ['effective_config' => ['instructions' => 'Changed approved context.']],
    ['task_context' => 'Different task.'],
    ['source_artifacts' => [['sha256' => str_repeat('a', 64)]]],
] as $index => $override) {
    $tests['session binding mismatch reconstructs fresh context '.$index] = function () use ($override): void {
        $transport = new SimulatedAgentTransport;
        $claim = driver_claim();
        $session = new AgentSession('0199a213-81c0-7800-8aa1-bbab2a035a53', AgentSession::binding(driver_claim($override)));

        (new CodexDriver)->start($claim, $transport, static function (): void {}, $session);

        driver_assert(! in_array('resume', $transport->commands[0]['argv'], true));
        driver_assert($transport->commands[0]['stdin'] === $claim->manifest['task_context']);
    };
}
foreach ([
    ['kind' => 'review', 'base_sha' => str_repeat('a', 40)],
    ['kind' => 'implement', 'base_sha' => str_repeat('b', 40)],
] as $index => $override) {
    $tests['patch output cannot misrepresent its source baseline '.$index] = function () use ($override): void {
        $transport = new SimulatedAgentTransport;
        $transport->stdout = str_replace('no_findings', 'changes_proposed', $transport->stdout);
        $transport->diff = "diff --git a/example.txt b/example.txt\n--- a/example.txt\n+++ b/example.txt\n@@ -1 +1 @@\n-before\n+after\n";

        driver_rejects(fn () => (new CodexDriver)->start(
            driver_claim($override),
            $transport,
            static function (): void {},
        ), 'invalid_result');
    };
}
$tests['flag-like model input cannot change native invocation'] = function (): void {
    $transport = new SimulatedAgentTransport;
    driver_rejects(fn () => (new CodexDriver)->start(driver_claim(['effective_config' => ['model' => '--oss']]), $transport, static function (): void {}));
    driver_assert($transport->commands === []);
};
$tests['stale authorization prevents native invocation'] = function (): void {
    $transport = new SimulatedAgentTransport;
    driver_rejects(fn () => (new CodexDriver)->start(driver_claim(), $transport, static function (): void {
        throw new RuntimeException('Stale fence.');
    }));
    driver_assert($transport->commands === []);
};
$tests['only the explicit run can load its protected session record'] = function (): void {
    $root = realpath(sys_get_temp_dir()).'/shipmunk-session-'.bin2hex(random_bytes(8));
    $store = new AgentSessionStore($root);
    $claim = driver_claim();
    $session = new AgentSession('0199a213-81c0-7800-8aa1-bbab2a035a53', AgentSession::binding($claim));

    try {
        driver_assert($store->read($claim) === null);
        $store->write($claim, $session);
        driver_assert($store->read($claim)?->id === $session->id);
        driver_assert($store->read(driver_claim(['head_sha' => str_repeat('f', 40)])) === null);
        chmod($root.'/'.$claim->runId.'.json', 0644);
        driver_rejects(fn () => $store->read($claim));
    } finally {
        unlink($root.'/'.$claim->runId.'.json');
        rmdir($root);
    }
};
$tests['trusted bundle enforces provenance and hashes independently of candidate files'] = function (): void {
    $root = realpath(sys_get_temp_dir()).'/shipmunk-instructions-'.bin2hex(random_bytes(8));
    mkdir($root.'/instructions', 0700, true);
    $claim = driver_claim();
    $configuration = $claim->manifest['effective_config'];
    $bundle = [
        'version' => 1,
        'trusted_revision' => $configuration['trusted_revision'],
        'effective_configuration_sha256' => $configuration['effective_configuration_sha256'],
        'effective_instructions' => ['contents' => $configuration['instructions'], 'sha256' => $configuration['instructions_sha256']],
        'trusted_files' => ['AGENTS.md' => ['path' => 'AGENTS.md', 'contents' => 'Approved agents.', 'sha256' => hash('sha256', 'Approved agents.')]],
    ];

    try {
        file_put_contents($root.'/instructions/0.json', json_encode($bundle));
        driver_assert((new TrustedAgentInstructions)->read($claim, $root) === 'Approved agents.');
        $bundle['trusted_files']['AGENTS.md']['contents'] = 'Tampered.';
        file_put_contents($root.'/instructions/0.json', json_encode($bundle));
        driver_rejects(fn () => (new TrustedAgentInstructions)->read($claim, $root));
    } finally {
        unlink($root.'/instructions/0.json');
        rmdir($root.'/instructions');
        rmdir($root);
    }
};

$failed = 0;
foreach ($tests as $name => $test) {
    try {
        $test();
        echo 'PASS '.$name."\n";
    } catch (Throwable $exception) {
        $failed++;
        fwrite(STDERR, 'FAIL '.$name.': '.$exception->getMessage()."\n");
    }
}
exit($failed === 0 ? 0 : 1);
