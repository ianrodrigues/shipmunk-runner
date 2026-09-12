<?php

declare(strict_types=1);

use Shipmunk\Runner\Claim;
use Shipmunk\Runner\CommandResult;
use Shipmunk\Runner\CommandRunner;
use Shipmunk\Runner\Drivers\AgentTransport;
use Shipmunk\Runner\Drivers\CodexDriver;
use Shipmunk\Runner\Drivers\CodexEventParser;
use Shipmunk\Runner\Drivers\DriverFailure;
use Shipmunk\Runner\Drivers\NativeFailureReason;

require dirname(__DIR__).'/bootstrap.php';

$image = getenv('SHIPMUNK_CODEX_BOUNDARY_IMAGE') ?: 'shipmunk-profile-native:local';
$runner = dirname(__DIR__);
$commands = new CommandRunner;
$capture = new class implements AgentTransport
{
    public array $argv = [];

    public function run(
        array $argv,
        string $stdin,
        Closure $checkpoint,
    ): CommandResult {
        $this->argv = $argv;

        throw new LogicException('Command captured without execution.');
    }

    public function patch(): ?string
    {
        return null;
    }

    public function changedFiles(): array
    {
        return [];
    }

    public function stop(): void {}
};
$claim = new Claim(
    '01kkkkkkkkkkkkkkkkkkkkkkkk',
    '01nnnnnnnnnnnnnnnnnnnnnnnn',
    1,
    new DateTimeImmutable('+45 seconds'),
    new DateTimeImmutable('+10 minutes'),
    [
        'protocol_version' => '1.0',
        'agent' => 'codex',
        'runtime_version' => '0.154.0',
        'effective_config' => [
            'model' => 'gpt-5.4',
            'instructions' => 'Offline approved instruction fixture.',
        ],
        'task_context' => 'Offline boundary fixture.',
    ],
);

try {
    (new CodexDriver)->start($claim, $capture, static fn () => null);
} catch (LogicException $exception) {
    if ($capture->argv === []) {
        throw $exception;
    }
}

$container = 'shipmunk-codex-boundary-'.bin2hex(random_bytes(8));
$command = [
    'docker', 'run', '--rm', '--name', $container,
    '--network', 'none', '--read-only', '--user', '65532:65532',
    '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--log-driver', 'none',
    '--pids-limit', '128', '--memory', '512m', '--cpus', '1',
    '--tmpfs', '/tmp:rw,nosuid,nodev,size=64m,mode=1777',
    '--tmpfs', '/profile:rw,nosuid,nodev,size=64m,uid=65532,gid=65532,mode=0700',
    '--tmpfs', '/bridge:rw,nosuid,nodev,noexec,size=1m,uid=65532,gid=65532,mode=0700',
    '--mount', 'type=bind,src='.$runner.'/containers/codex-mcp.mjs,dst=/fixture/codex-mcp.mjs,readonly',
    '--mount', 'type=bind,src='.$runner.'/containers/codex-mcp.mjs,dst=/usr/local/lib/shipmunk/codex-mcp.mjs,readonly',
    '--mount', 'type=bind,src='.$runner.'/containers/codex-result.schema.json,dst=/usr/local/lib/shipmunk/codex-result.schema.json,readonly',
    '--mount', 'type=bind,src='.$runner.'/tests/fixtures/codex-boundary,dst=/fixture/tests,readonly',
    '--entrypoint', '/usr/bin/env', $image,
    '-i', 'PATH=/usr/local/bin:/usr/bin:/bin', 'HOME=/profile', 'CODEX_HOME=/profile/.codex',
    'SHIPMUNK_FIXTURE_ARGV='.json_encode($capture->argv, JSON_THROW_ON_ERROR),
    '/usr/local/bin/node', '/fixture/tests/native.mjs',
];

try {
    $result = $commands->mustRun($command, timeoutSeconds: 210);
    $rejections = [];

    foreach (explode("\n", rtrim($result->stdout, "\n")) as $line) {
        if (! str_starts_with($line, 'NATIVE_REJECTION_FIXTURE ')) {
            fwrite(STDOUT, $line."\n");

            continue;
        }

        $fixture = json_decode(substr($line, 25), true, 32, JSON_THROW_ON_ERROR);
        $expected = match ($fixture['backend_code']) {
            'model_not_found' => NativeFailureReason::ModelUnavailable,
            'invalid_json_schema' => NativeFailureReason::InvalidOutputSchema,
            'unknown' => NativeFailureReason::ProcessError,
        };

        try {
            (new CodexEventParser)->parse(
                $claim,
                $fixture['exit_code'],
                $fixture['stdout'],
                $fixture['stderr'],
            );

            throw new LogicException('Native backend rejection was accepted.');
        } catch (DriverFailure $failure) {
            if ($failure->reason !== $expected) {
                throw new LogicException('Native backend rejection lost its known reason.');
            }

            if (str_contains($failure->summary(), 'SYNTHETIC_SECRET')) {
                throw new LogicException('Native backend rejection leaked provider text.');
            }
        }

        $rejections[] = $fixture['backend_code'];
        fwrite(STDOUT, 'PASS pinned native backend rejection '.$expected->value."\n");
    }

    if ($rejections !== ['model_not_found', 'invalid_json_schema', 'unknown']) {
        throw new LogicException('Native backend rejection fixtures were not all exercised.');
    }
} finally {
    $commands->run(['docker', 'rm', '--force', $container]);
}
