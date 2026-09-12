<?php

declare(strict_types=1);

use Shipmunk\Runner\AgentInputSanitizer;
use Shipmunk\Runner\AttemptStateStore;
use Shipmunk\Runner\Claim;
use Shipmunk\Runner\Clock;
use Shipmunk\Runner\CommandRunner;
use Shipmunk\Runner\ControlPlaneClient;
use Shipmunk\Runner\ControlPlaneException;
use Shipmunk\Runner\DockerSandbox;
use Shipmunk\Runner\DockerSandboxProcess;
use Shipmunk\Runner\Drivers\CodexWatchdog;
use Shipmunk\Runner\FakeNativeExecutableAdapter;
use Shipmunk\Runner\Heartbeat;
use Shipmunk\Runner\HostWatchdog;
use Shipmunk\Runner\HttpControlPlaneClient;
use Shipmunk\Runner\HttpResponse;
use Shipmunk\Runner\HttpTransport;
use Shipmunk\Runner\NativeExecutableAdapter;
use Shipmunk\Runner\NativeExecution;
use Shipmunk\Runner\Protocol;
use Shipmunk\Runner\RunCommand;
use Shipmunk\Runner\SafeTarExtractor;
use Shipmunk\Runner\Sandbox;
use Shipmunk\Runner\SandboxProcess;
use Shipmunk\Runner\Supervisor;
use Shipmunk\Runner\Watchdog;
use Shipmunk\Runner\WatchdogLease;
use Shipmunk\Runner\WorkspacePreparer;

require dirname(__DIR__).'/bootstrap.php';

$tests = [];

function runner_test(string $name, Closure $test): void
{
    global $tests;
    $tests[$name] = $test;
}

function assert_true(bool $condition, string $message = 'Assertion failed.'): void
{
    if (! $condition) {
        throw new RuntimeException($message);
    }
}

function assert_same(mixed $expected, mixed $actual, string $message = ''): void
{
    if ($expected !== $actual) {
        throw new RuntimeException($message !== '' ? $message : sprintf(
            'Expected %s, got %s.',
            var_export($expected, true),
            var_export($actual, true),
        ));
    }
}

function assert_throws(string $class, Closure $callback): void
{
    try {
        $callback();
    } catch (Throwable $exception) {
        assert_true($exception instanceof $class, "Expected {$class}, got ".get_class($exception).'.');

        return;
    }

    throw new RuntimeException("Expected {$class} to be thrown.");
}

function temporary_directory(): string
{
    $directory = sys_get_temp_dir().'/shipmunk-runner-test-'.bin2hex(random_bytes(6));

    if (! mkdir($directory, 0700)) {
        throw new RuntimeException('Unable to create test directory.');
    }

    return $directory;
}

function remove_directory(string $directory): void
{
    if (! is_dir($directory)) {
        return;
    }

    $iterator = new RecursiveIteratorIterator(
        new RecursiveDirectoryIterator($directory, FilesystemIterator::SKIP_DOTS),
        RecursiveIteratorIterator::CHILD_FIRST,
    );

    foreach ($iterator as $entry) {
        $entry->isDir() && ! $entry->isLink() ? rmdir($entry->getPathname()) : unlink($entry->getPathname());
    }

    rmdir($directory);
}

function tar_archive(string $name, string $contents = '', string $type = '0', string $link = ''): string
{
    $field = static fn (string $value, int $length): string => str_pad(substr($value, 0, $length), $length, "\0");
    $header = $field($name, 100)
        .sprintf('%07o', 0600)."\0"
        .sprintf('%07o', 65532)."\0"
        .sprintf('%07o', 65532)."\0"
        .sprintf('%011o', strlen($contents))."\0"
        .sprintf('%011o', 0)."\0"
        .str_repeat(' ', 8)
        .$type
        .$field($link, 100)
        ."ustar\0"
        .'00'
        .$field('runner', 32)
        .$field('runner', 32)
        .sprintf('%07o', 0)."\0"
        .sprintf('%07o', 0)."\0"
        .$field('', 155)
        .str_repeat("\0", 12);
    $checksum = array_sum(unpack('C*', $header));
    $header = substr_replace($header, sprintf('%06o', $checksum)."\0 ", 148, 8);
    $padding = (512 - (strlen($contents) % 512)) % 512;

    return $header.$contents.str_repeat("\0", $padding).str_repeat("\0", 1024);
}

function fixture_claim(array $manifest = [], ?string $attemptId = null): Claim
{
    return new Claim(
        '01k4w000000000000000000001',
        $attemptId ?? '01k4w000000000000000000002',
        1,
        new DateTimeImmutable('@1045'),
        new DateTimeImmutable('@1900'),
        array_merge([
            'protocol_version' => '1.0',
            'source_artifacts' => [],
            'instruction_artifacts' => [],
        ], $manifest),
    );
}

/** @return array{source: array{artifact_id: string, sha256: string}, instruction: array{artifact_id: string, sha256: string}} */
function artifact_references(string $archive): array
{
    return [
        'source' => [
            'artifact_id' => '01k4w000000000000000000003',
            'sha256' => hash('sha256', $archive),
        ],
        'instruction' => [
            'artifact_id' => '01k4w000000000000000000004',
            'sha256' => hash('sha256', '{"trusted":true}'),
        ],
    ];
}

final class RecordingTransport implements HttpTransport
{
    /** @var list<HttpResponse> */
    public array $responses = [];

    /** @var list<array{method: string, url: string, headers: array<string, string>, body: string, timeoutSeconds: int, maxResponseBytes: int}> */
    public array $requests = [];

    public function request(string $method, string $url, array $headers, string $body, int $timeoutSeconds, int $maxResponseBytes): HttpResponse
    {
        $this->requests[] = compact('method', 'url', 'headers', 'body', 'timeoutSeconds', 'maxResponseBytes');

        return array_shift($this->responses) ?? throw new RuntimeException('Missing fake HTTP response.');
    }
}

final class FakeClock implements Clock
{
    public function __construct(public float $time = 1000.0) {}

    public function now(): float
    {
        return $this->time;
    }

    public function sleepMilliseconds(int $milliseconds): void
    {
        $this->time += $milliseconds / 1000;
    }
}

final class FakeProcess implements SandboxProcess
{
    public bool $started = false;

    public bool $stopped = false;

    public bool $removed = false;

    public function __construct(public bool $running = true, public string $nativeOutput = '') {}

    public function id(): string
    {
        return str_repeat('a', 64);
    }

    public function start(?Closure $checkpoint = null): void
    {
        $this->started = true;
        $checkpoint?->__invoke();
    }

    public function isRunning(): bool
    {
        return $this->running && ! $this->stopped;
    }

    public function exitCode(): ?int
    {
        return $this->isRunning() ? null : 0;
    }

    public function output(): string
    {
        return $this->nativeOutput;
    }

    public function stop(): void
    {
        $this->stopped = true;
    }

    public function remove(): void
    {
        $this->removed = true;
        $this->stop();
    }
}

final class FakeSandbox implements Sandbox
{
    /** @var list<string> */
    public array $reconciled = [];

    public function __construct(public FakeProcess $process) {}

    public function create(Claim $claim, array $agentInput, string $workspace): SandboxProcess
    {
        return $this->process;
    }

    public function reconcile(string $sandboxId): void
    {
        $this->reconciled[] = $sandboxId;
    }
}

final class FakeWatchdogLease implements WatchdogLease
{
    /** @var list<DateTimeImmutable> */
    public array $renewals = [];

    public bool $disarmed = false;

    public function renew(DateTimeImmutable $lease): void
    {
        $this->renewals[] = $lease;
    }

    public function disarm(): void
    {
        $this->disarmed = true;
    }
}

final class FakeWatchdog implements Watchdog
{
    public FakeWatchdogLease $lease;

    public function __construct()
    {
        $this->lease = new FakeWatchdogLease;
    }

    public function arm(string $sandboxId, DateTimeImmutable $lease, DateTimeImmutable $deadline): WatchdogLease
    {
        return $this->lease;
    }
}

final class FakeControlPlane implements ControlPlaneClient
{
    public int $claims = 0;

    public bool $hasWork = true;

    public int $heartbeatCount = 0;

    public bool $completed = false;

    /** @var array<string, mixed>|null */
    public ?array $completedResult = null;

    /** @var list<array{kind: string, bytes: string, sha256: string}> */
    public array $uploaded = [];

    /** @var list<array<string, mixed>> */
    public array $events = [];

    public bool $acknowledged = false;

    public bool $failAcknowledgement = false;

    public ?Closure $onAcknowledge = null;

    public ?Closure $onDownload = null;

    public ?Closure $onHeartbeat = null;

    public ?Closure $onComplete = null;

    public function __construct(
        public Claim $work,
        private string $archive,
        public ?int $failHeartbeatAt = null,
        private Clock $clock = new FakeClock,
    ) {}

    public function claim(): ?Claim
    {
        $this->claims++;

        return $this->hasWork ? $this->work : null;
    }

    public function heartbeat(Claim $claim): Heartbeat
    {
        $this->heartbeatCount++;
        $this->onHeartbeat?->__invoke();

        if ($this->failHeartbeatAt !== null && $this->heartbeatCount >= $this->failHeartbeatAt) {
            throw new RuntimeException('lease lost');
        }

        return new Heartbeat(false, new DateTimeImmutable('@'.(string) ((int) $this->clock->now() + 45)));
    }

    public function acknowledgeStopped(Claim $claim): void
    {
        $this->onAcknowledge?->__invoke();

        if ($this->failAcknowledgement) {
            throw new RuntimeException('ack failed');
        }

        $this->acknowledged = true;
    }

    public function sendEvents(Claim $claim, array $events): void
    {
        array_push($this->events, ...$events);
    }

    public function downloadArtifact(Claim $claim, string $artifactId, string $sha256): string
    {
        $this->onDownload?->__invoke();
        $bytes = $artifactId === '01k4w000000000000000000004'
            ? '{"trusted":true}'
            : $this->archive;
        assert_same(hash('sha256', $bytes), $sha256);

        return $bytes;
    }

    public function uploadArtifact(Claim $claim, string $kind, string $bytes, string $sha256): string
    {
        $this->uploaded[] = compact('kind', 'bytes', 'sha256');

        return '01k4w000000000000000000003';
    }

    public function complete(Claim $claim, array $result): void
    {
        $this->onComplete?->__invoke();
        $this->completed = true;
        $this->completedResult = $result;
    }
}

runner_test('HTTP client fences every attempt mutation and keeps bearer credentials in headers', function (): void {
    $transport = new RecordingTransport;
    $transport->responses = [
        new HttpResponse(204, [], ''),
        new HttpResponse(200, [], '{"data":{"protocol_version":"1.0","attempt_id":"01k4w000000000000000000002","fence":1,"state":"running","lease_expires_at":"1970-01-01T00:17:25Z","stop_requested":false,"stop_reason":null}}'),
        new HttpResponse(204, [], ''),
        new HttpResponse(201, [], '{"data":{"id":"01k4w000000000000000000003"}}'),
        new HttpResponse(204, [], ''),
    ];
    $client = new HttpControlPlaneClient('http://127.0.0.1', 'runner-secret', $transport);
    $claim = fixture_claim();

    assert_same(null, $client->claim());
    $client->heartbeat($claim);
    $client->sendEvents($claim, [[
        'protocol_version' => '1.0',
        'attempt_id' => $claim->attemptId,
        'fence' => 1,
        'sequence' => 1,
    ]]);
    $bytes = 'artifact';
    $client->uploadArtifact($claim, 'native_output', $bytes, hash('sha256', $bytes));
    $client->complete($claim, ['outcome' => 'incomplete']);

    assert_same(5, count($transport->requests));

    foreach (array_slice($transport->requests, 1) as $request) {
        assert_same('Bearer runner-secret', $request['headers']['Authorization']);
        assert_true(! str_contains($request['body'], 'runner-secret'));
    }

    assert_true(str_contains($transport->requests[1]['body'], '"fence":1'));
    assert_true(str_contains($transport->requests[2]['body'], '"fence":1'));
    assert_true(str_ends_with($transport->requests[3]['url'], '/artifacts'));
    assert_same('1', $transport->requests[3]['headers']['X-Attempt-Fence']);
    assert_same('1.0', $transport->requests[3]['headers']['X-Protocol-Version']);
    assert_true(str_contains($transport->requests[4]['body'], '"fence":1'));
});

runner_test('HTTP client rejects incompatible protocol before accepting a claim', function (): void {
    $transport = new RecordingTransport;
    $transport->responses[] = new HttpResponse(426, [], 'do not expose this body');
    $client = new HttpControlPlaneClient('https://shipmunk.invalid', 'runner-secret', $transport);

    assert_throws(ControlPlaneException::class, fn () => $client->claim());
});

runner_test('HTTP client accepts the flat manifest claim with a 45 second local lease', function (): void {
    $transport = new RecordingTransport;
    $manifest = [
        'protocol_version' => '1.0',
        'run_id' => '01k4w000000000000000000001',
        'attempt_id' => '01k4w000000000000000000002',
        'fence' => 1,
        'deadline' => '2099-01-01T00:00:00Z',
        'source_artifacts' => [],
        'instruction_artifacts' => [],
    ];
    $transport->responses[] = new HttpResponse(200, [], json_encode(['data' => $manifest], JSON_THROW_ON_ERROR));
    $client = new HttpControlPlaneClient('https://shipmunk.invalid', 'runner-secret', $transport);
    $before = time();

    $claim = $client->claim();

    assert_true($claim instanceof Claim);
    assert_same($manifest, $claim->manifest);
    assert_true($claim->leaseExpiresAt->getTimestamp() >= $before + Supervisor::LEASE_SECONDS);
    assert_true($claim->leaseExpiresAt->getTimestamp() <= time() + Supervisor::LEASE_SECONDS);
    assert_same(10, Supervisor::HEARTBEAT_SECONDS);
    assert_same(45, Supervisor::LEASE_SECONDS);
});

runner_test('claim identifiers reject trailing line breaks', function (): void {
    assert_throws(InvalidArgumentException::class, fn () => new Claim(
        "01k4w000000000000000000001\n",
        '01k4w000000000000000000002',
        1,
        new DateTimeImmutable('@1045'),
        new DateTimeImmutable('@1900'),
        ['protocol_version' => '1.0'],
    ));
});

runner_test('HTTP client downloads only attempt-scoped input artifacts and verifies both hashes', function (): void {
    $transport = new RecordingTransport;
    $bytes = 'source archive';
    $sha256 = hash('sha256', $bytes);
    $transport->responses[] = new HttpResponse(200, [
        'x-artifact-sha256' => $sha256,
        'content-length' => (string) strlen($bytes),
    ], $bytes);
    $client = new HttpControlPlaneClient('https://shipmunk.invalid', 'runner-secret', $transport);
    $claim = fixture_claim();

    assert_same($bytes, $client->downloadArtifact($claim, '01k4w000000000000000000003', $sha256));
    assert_true(str_ends_with(
        $transport->requests[0]['url'],
        '/runner/v1/attempts/01k4w000000000000000000002/input-artifacts/01k4w000000000000000000003',
    ));
    assert_true(! str_contains($transport->requests[0]['url'], '?'));
    assert_same('application/octet-stream', $transport->requests[0]['headers']['Accept']);
    assert_same(Protocol::INPUT_ARTIFACT_MAX_BYTES, $transport->requests[0]['maxResponseBytes']);
    assert_same(Protocol::HTTP_TIMEOUT_SECONDS, $transport->requests[0]['timeoutSeconds']);

    $transport->responses[] = new HttpResponse(200, [
        'x-artifact-sha256' => $sha256,
    ], $bytes);
    assert_same($bytes, $client->downloadArtifact(
        $claim,
        '01k4w000000000000000000003',
        $sha256,
    ));

    $transport->responses[] = new HttpResponse(200, [
        'x-artifact-sha256' => $sha256,
        'content-length' => 'invalid',
    ], $bytes);
    assert_throws(RuntimeException::class, fn () => $client->downloadArtifact(
        $claim,
        '01k4w000000000000000000003',
        $sha256,
    ));

    $transport->responses[] = new HttpResponse(200, [
        'x-artifact-sha256' => str_repeat('0', 64),
        'content-length' => (string) strlen($bytes),
    ], $bytes);
    assert_throws(RuntimeException::class, fn () => $client->downloadArtifact(
        $claim,
        '01k4w000000000000000000003',
        $sha256,
    ));
});

runner_test('HTTP client acknowledges stopped isolation through the fenced heartbeat endpoint', function (): void {
    $transport = new RecordingTransport;
    $transport->responses[] = new HttpResponse(204, [], '');
    $client = new HttpControlPlaneClient('https://shipmunk.invalid', 'runner-secret', $transport);
    $claim = fixture_claim();

    $client->acknowledgeStopped($claim);

    assert_true(str_ends_with($transport->requests[0]['url'], "/attempts/{$claim->attemptId}/heartbeat"));
    assert_same([
        'protocol_version' => '1.0',
        'attempt_id' => $claim->attemptId,
        'fence' => 1,
        'stopped' => true,
    ], json_decode($transport->requests[0]['body'], true, flags: JSON_THROW_ON_ERROR));
});

runner_test('agent input recursively removes supervisor and credential fields', function (): void {
    $clean = (new AgentInputSanitizer)->sanitize([
        'task' => ['name' => 'safe', 'github_token' => 'secret'],
        'supervisor' => ['credential_reference' => 'credential:one'],
        'APP_KEY' => 'secret',
        'runner_token' => 'secret',
    ]);

    assert_same(['task' => ['name' => 'safe']], $clean);
});

runner_test('malformed native output cannot produce a successful completion', function (): void {
    $adapter = new FakeNativeExecutableAdapter;
    $claim = fixture_claim();

    assert_throws(RuntimeException::class, fn () => $adapter->decode($claim, 0, 'not-json'));

    $output = json_encode(['result' => [
        'protocol_version' => '1.0',
        'run_id' => $claim->runId,
        'attempt_id' => $claim->attemptId,
        'fence' => $claim->fence,
        'summary' => 'claimed success',
        'outcome' => 'no_findings',
        'findings' => [],
        'patch_artifact' => null,
        'tests' => [],
        'usage' => null,
    ]], JSON_THROW_ON_ERROR);
    $execution = $adapter->decode($claim, 1, $output);

    assert_same('incomplete', $execution->result['outcome']);
});

runner_test('safe tar extraction accepts regular files and rejects traversal and links', function (): void {
    $directory = temporary_directory();

    try {
        $extractor = new SafeTarExtractor;
        $extractor->extract(tar_archive('src/file.txt', 'safe'), $directory);
        assert_same('safe', file_get_contents($directory.'/src/file.txt'));
        $extractor->extract(gzencode(tar_archive('compressed.txt', 'safe')), $directory);
        assert_same('safe', file_get_contents($directory.'/compressed.txt'));
        assert_throws(RuntimeException::class, fn () => $extractor->extract(tar_archive('../escape', 'bad'), $directory));
        assert_throws(RuntimeException::class, fn () => $extractor->extract(tar_archive('link', '', '2', '../escape'), $directory));
        assert_throws(RuntimeException::class, fn () => (new SafeTarExtractor(maxBytes: 10))->extract(
            gzencode(tar_archive('large', str_repeat('x', 100))),
            $directory,
        ));
        assert_true(! file_exists(dirname($directory).'/escape'));
    } finally {
        remove_directory($directory);
    }
});

runner_test('sandbox startup reports cleanup failure without replacing its original cause', function (): void {
    $directory = temporary_directory();
    $bin = $directory.'/bin';
    mkdir($bin, 0700);
    file_put_contents($bin.'/docker', <<<'SH'
#!/bin/sh
case "$1" in
    start) echo "startup command detail" >&2; exit 1 ;;
    inspect) echo "true"; exit 0 ;;
    stop) echo "cleanup stop detail" >&2; exit 1 ;;
    kill) echo "cleanup kill detail" >&2; exit 1 ;;
esac
exit 1
SH);
    chmod($bin.'/docker', 0700);
    $path = getenv('PATH');
    putenv('PATH='.$bin.':'.($path === false ? '' : $path));

    try {
        (new DockerSandboxProcess(str_repeat('a', 64), new CommandRunner, $directory, []))->start();
        throw new RuntimeException('Expected sandbox startup to fail.');
    } catch (RuntimeException $exception) {
        assert_same('Sandbox startup failed, and its cleanup also failed.', $exception->getMessage());
        assert_true(! str_contains($exception->getMessage(), 'cleanup kill detail'));
        assert_true(str_contains($exception->getPrevious()?->getMessage() ?? '', 'startup command detail'));
    } finally {
        putenv($path === false ? 'PATH' : 'PATH='.$path);
        remove_directory($directory);
    }
});

runner_test('command runner still times out an idle subprocess', function (): void {
    $started = microtime(true);
    assert_throws(RuntimeException::class, fn () => (new CommandRunner)->mustRun(
        [PHP_BINARY, '-r', 'sleep(5);'],
        timeoutSeconds: 1,
    ));
    assert_true(microtime(true) - $started < 3, 'Idle subprocess timeout was not enforced promptly.');
});

runner_test('Codex watchdog rollback disarms every armed sibling and preserves the arm failure', function (): void {
    $record = (object) ['disarmed' => []];
    $watchdog = new class($record) implements Watchdog
    {
        public function __construct(private object $record) {}

        public function arm(string $sandboxId, DateTimeImmutable $lease, DateTimeImmutable $deadline): WatchdogLease
        {
            if (str_ends_with($sandboxId, '-diff')) {
                throw new RuntimeException('diff watchdog arm failed');
            }

            return new class($this->record, $sandboxId) implements WatchdogLease
            {
                public function __construct(private object $record, private string $sandboxId) {}

                public function renew(DateTimeImmutable $lease): void {}

                public function disarm(): void
                {
                    $this->record->disarmed[] = $this->sandboxId;

                    if ($this->sandboxId === 'sandbox') {
                        throw new RuntimeException('first rollback failed');
                    }
                }
            };
        }
    };

    try {
        (new CodexWatchdog($watchdog))->arm(
            'sandbox',
            new DateTimeImmutable('@1045'),
            new DateTimeImmutable('@1900'),
        );
        throw new RuntimeException('Expected watchdog arm to fail.');
    } catch (RuntimeException $exception) {
        assert_same('diff watchdog arm failed', $exception->getMessage());
    }

    assert_same(['sandbox', 'sandbox-repo'], $record->disarmed);
});

runner_test('deadline crossing during heartbeat blocks the renewed attempt', function (): void {
    $directory = temporary_directory();
    $clock = new FakeClock;
    $claim = new Claim(
        '01k4w000000000000000000001',
        '01k4w000000000000000000002',
        1,
        new DateTimeImmutable('@1045'),
        new DateTimeImmutable('@1001'),
        [
            'protocol_version' => '1.0',
            'source_artifacts' => [],
            'instruction_artifacts' => [],
        ],
    );
    $client = new FakeControlPlane($claim, '', clock: $clock);
    $client->onHeartbeat = function () use ($clock): void {
        $clock->time = 1001;
    };
    $process = new FakeProcess(running: false);
    $supervisor = new Supervisor(
        $client,
        new FakeSandbox($process),
        new FakeNativeExecutableAdapter,
        new AttemptStateStore($directory.'/active.json'),
        new WorkspacePreparer($directory.'/workspaces'),
        clock: $clock,
        watchdog: new FakeWatchdog,
    );

    try {
        assert_throws(RuntimeException::class, fn () => $supervisor->runOnce());
        assert_same(1, $client->heartbeatCount);
        assert_true(! $process->started && ! $client->completed);
    } finally {
        remove_directory($directory);
    }
});

runner_test('deadline crossing during decode blocks every publication request', function (): void {
    $directory = temporary_directory();
    $clock = new FakeClock;
    $claim = new Claim(
        '01k4w000000000000000000001',
        '01k4w000000000000000000002',
        1,
        new DateTimeImmutable('@1045'),
        new DateTimeImmutable('@1001'),
        [
            'protocol_version' => '1.0',
            'source_artifacts' => [],
            'instruction_artifacts' => [],
        ],
    );
    $client = new FakeControlPlane($claim, '', clock: $clock);
    $adapter = new class($clock) implements NativeExecutableAdapter
    {
        public function __construct(private FakeClock $clock) {}

        public function decode(Claim $claim, int $exitCode, string $output): NativeExecution
        {
            $this->clock->time = 1001;
            $bytes = 'late patch';

            return new NativeExecution(
                [['type' => 'progress']],
                [['kind' => 'patch', 'bytes' => $bytes, 'sha256' => hash('sha256', $bytes)]],
                [
                    'protocol_version' => '1.0',
                    'run_id' => $claim->runId,
                    'attempt_id' => $claim->attemptId,
                    'fence' => $claim->fence,
                    'patch_artifact' => null,
                ],
            );
        }
    };
    $process = new FakeProcess(running: false);
    $supervisor = new Supervisor(
        $client,
        new FakeSandbox($process),
        $adapter,
        new AttemptStateStore($directory.'/active.json'),
        new WorkspacePreparer($directory.'/workspaces'),
        clock: $clock,
        watchdog: new FakeWatchdog,
    );

    try {
        assert_throws(RuntimeException::class, fn () => $supervisor->runOnce());
        assert_same([], $client->events);
        assert_same([], $client->uploaded);
        assert_true(! $client->completed && $client->acknowledged);
    } finally {
        remove_directory($directory);
    }
});

runner_test('lease heartbeat failure stops and removes the sandbox before clearing state', function (): void {
    $directory = temporary_directory();
    $archive = tar_archive('file.txt', 'safe');
    $references = artifact_references($archive);
    $claim = fixture_claim([
        'source_artifacts' => [$references['source']],
        'instruction_artifacts' => [$references['instruction']],
    ]);
    $clock = new FakeClock;
    $client = new FakeControlPlane($claim, $archive, failHeartbeatAt: 12, clock: $clock);
    $process = new FakeProcess;
    $sandbox = new FakeSandbox($process);
    $state = new AttemptStateStore($directory.'/active.json');
    $supervisor = new Supervisor(
        $client,
        $sandbox,
        new FakeNativeExecutableAdapter,
        $state,
        new WorkspacePreparer($directory.'/workspaces'),
        clock: $clock,
        watchdog: new FakeWatchdog,
    );

    try {
        assert_throws(RuntimeException::class, fn () => $supervisor->runOnce());
        assert_same(12, $client->heartbeatCount);
        assert_true($process->started && $process->stopped && $process->removed);
        assert_true(! $client->completed);
        assert_true($client->acknowledged);
        assert_same(null, $state->load());
    } finally {
        remove_directory($directory);
    }
});

runner_test('restart reconciliation removes the recorded sandbox before another claim', function (): void {
    $directory = temporary_directory();
    $claim = fixture_claim();
    $process = new FakeProcess;
    $state = new AttemptStateStore($directory.'/active.json');
    $state->save($claim, $process, $claim->leaseExpiresAt, $directory.'/workspace');
    $sandbox = new FakeSandbox($process);
    $client = new FakeControlPlane($claim, '');
    $supervisor = new Supervisor(
        $client,
        $sandbox,
        new FakeNativeExecutableAdapter,
        $state,
        new WorkspacePreparer($directory.'/workspaces'),
        watchdog: new FakeWatchdog,
    );

    try {
        $supervisor->reconcileAfterRestart();
        assert_same([$process->id()], $sandbox->reconciled);
        assert_true($client->acknowledged);
        assert_same(null, $state->load());
    } finally {
        remove_directory($directory);
    }
});

runner_test('workspace cleanup failure retains state and withholds stopped acknowledgement', function (): void {
    $directory = temporary_directory();
    $claim = fixture_claim();
    $workspaces = new WorkspacePreparer($directory.'/workspaces');
    $workspace = $workspaces->path($claim);
    mkdir($workspace, 0700, true);
    file_put_contents($workspace.'/locked', 'data');
    chmod($workspace, 0500);
    $state = new AttemptStateStore($directory.'/active.json');
    $state->save($claim, null, $claim->leaseExpiresAt, $workspace);
    $client = new FakeControlPlane($claim, '');
    $supervisor = new Supervisor(
        $client,
        new FakeSandbox(new FakeProcess),
        new FakeNativeExecutableAdapter,
        $state,
        $workspaces,
        watchdog: new FakeWatchdog,
    );

    try {
        assert_throws(RuntimeException::class, fn () => $supervisor->reconcileAfterRestart());
        assert_true($state->load() !== null);
        assert_true(! $client->acknowledged);
    } finally {
        chmod($workspace, 0700);
        remove_directory($directory);
    }
});

runner_test('failed cleanup acknowledgement retains state and blocks replacement until restart reconciliation', function (): void {
    $directory = temporary_directory();
    $archive = tar_archive('file.txt', 'safe');
    $references = artifact_references($archive);
    $claim = fixture_claim([
        'source_artifacts' => [$references['source']],
        'instruction_artifacts' => [$references['instruction']],
    ]);
    $clock = new FakeClock;
    $client = new FakeControlPlane($claim, $archive, failHeartbeatAt: 12, clock: $clock);
    $client->failAcknowledgement = true;
    $process = new FakeProcess;
    $sandbox = new FakeSandbox($process);
    $state = new AttemptStateStore($directory.'/active.json');
    $client->onAcknowledge = function () use ($process, $state): void {
        assert_true($process->removed, 'Stopped acknowledgement preceded sandbox removal.');
        assert_true($state->load() !== null, 'State was cleared before stopped acknowledgement.');
    };
    $supervisor = new Supervisor(
        $client,
        $sandbox,
        new FakeNativeExecutableAdapter,
        $state,
        new WorkspacePreparer($directory.'/workspaces'),
        clock: $clock,
        watchdog: new FakeWatchdog,
    );

    try {
        assert_throws(RuntimeException::class, fn () => $supervisor->runOnce());
        assert_true($process->removed);
        assert_true($state->load() !== null, 'State was cleared before stopped acknowledgement.');

        $client->failAcknowledgement = false;
        $supervisor->reconcileAfterRestart();

        assert_same([$process->id()], $sandbox->reconciled);
        assert_true($client->acknowledged);
        assert_same(null, $state->load());
    } finally {
        remove_directory($directory);
    }
});

runner_test('successful completion cleans up and acknowledges stopped before clearing state', function (): void {
    $directory = temporary_directory();
    $archive = tar_archive('file.txt', 'safe');
    $references = artifact_references($archive);
    $claim = fixture_claim([
        'source_artifacts' => [$references['source']],
        'instruction_artifacts' => [$references['instruction']],
    ]);
    $output = json_encode(['events' => [], 'artifacts' => [], 'result' => [
        'protocol_version' => '1.0',
        'run_id' => $claim->runId,
        'attempt_id' => $claim->attemptId,
        'fence' => $claim->fence,
        'summary' => 'done',
        'outcome' => 'incomplete',
        'findings' => [],
        'patch_artifact' => null,
        'tests' => [],
        'usage' => null,
    ]], JSON_THROW_ON_ERROR);
    $clock = new FakeClock;
    $client = new FakeControlPlane($claim, $archive, clock: $clock);
    $process = new FakeProcess(running: false, nativeOutput: $output);
    $state = new AttemptStateStore($directory.'/active.json');
    $client->onAcknowledge = function () use ($client, $process, $state): void {
        assert_true($client->completed, 'Stopped acknowledgement preceded completion.');
        assert_true($process->removed, 'Stopped acknowledgement preceded sandbox removal.');
        assert_true($state->load() !== null, 'State was cleared before stopped acknowledgement.');
    };
    $supervisor = new Supervisor(
        $client,
        new FakeSandbox($process),
        new FakeNativeExecutableAdapter,
        $state,
        new WorkspacePreparer($directory.'/workspaces'),
        clock: $clock,
        watchdog: new FakeWatchdog,
    );

    try {
        assert_same(true, $supervisor->runOnce());
        assert_true($client->completed);
        assert_true($process->removed);
        assert_true($client->acknowledged);
        assert_same(null, $state->load());
    } finally {
        remove_directory($directory);
    }
});

foreach ([
    'no_findings' => 0,
    'incomplete' => 1,
    'needs_input' => 1,
    'no_work' => 0,
    'rejected_completion' => 1,
    'rejected_authorization' => 1,
    'unconfirmed_cleanup' => 1,
] as $case => $expectedExit) {
    runner_test('poll once reports '.$case.' only after accepted completion and cleanup', function () use (
        $case,
        $expectedExit,
    ): void {
        $directory = temporary_directory();
        $archive = tar_archive('file.txt', 'safe');
        $references = artifact_references($archive);
        $claim = fixture_claim([
            'source_artifacts' => [$references['source']],
            'instruction_artifacts' => [$references['instruction']],
        ]);
        $outcome = in_array($case, ['no_findings', 'incomplete', 'needs_input'], true) ? $case : 'no_findings';
        $nativeOutput = json_encode([
            'events' => [],
            'artifacts' => [],
            'result' => [
                'protocol_version' => '1.0',
                'run_id' => $claim->runId,
                'attempt_id' => $claim->attemptId,
                'fence' => $claim->fence,
                'summary' => 'SYNTHETIC_PRIVATE_MODEL_CONTENT',
                'outcome' => $outcome,
                'findings' => [],
                'patch_artifact' => null,
                'tests' => [],
                'usage' => null,
            ],
        ], JSON_THROW_ON_ERROR);
        $clock = new FakeClock;
        $client = new FakeControlPlane($claim, $archive, clock: $clock);
        $client->hasWork = $case !== 'no_work';
        $client->failAcknowledgement = $case === 'unconfirmed_cleanup';
        $process = new FakeProcess(running: false, nativeOutput: $nativeOutput);
        $state = new AttemptStateStore($directory.'/active.json');
        $terminal = [];
        $errors = [];
        $client->onComplete = static function () use ($case, &$terminal): void {
            assert_same([], $terminal, 'Completion was reported before the server accepted it.');

            if ($case === 'rejected_completion') {
                throw new RuntimeException('SYNTHETIC_PRIVATE_RESPONSE');
            }

            if ($case === 'rejected_authorization') {
                throw new ControlPlaneException(401, 'SYNTHETIC_PRIVATE_RESPONSE');
            }
        };
        $client->onAcknowledge = static function () use (&$terminal): void {
            assert_same([], $terminal, 'Completion was reported before stopped acknowledgement.');
        };
        $supervisor = new Supervisor(
            $client,
            new FakeSandbox($process),
            new FakeNativeExecutableAdapter,
            $state,
            new WorkspacePreparer($directory.'/workspaces'),
            clock: $clock,
            watchdog: new FakeWatchdog,
        );
        $command = new RunCommand(
            $supervisor,
            static function (string $message) use (&$terminal, $case, $client, $process, $state): void {
                if ($case !== 'no_work') {
                    assert_true($client->completed && $client->acknowledged);
                    assert_true($process->removed);
                    assert_same(null, $state->load());
                }

                $terminal[] = $message;
            },
            static function (string $message) use (&$errors): void {
                $errors[] = $message;
            },
        );

        try {
            assert_same($expectedExit, $command->run(true));
            assert_same(1, $client->claims, 'Poll once claimed more than one attempt.');

            if (in_array($case, ['rejected_completion', 'rejected_authorization', 'unconfirmed_cleanup'], true)) {
                assert_same([], $terminal);
                assert_same(1, count($errors));
                assert_same($case === 'unconfirmed_cleanup', $state->load() !== null);

                if ($case === 'rejected_authorization') {
                    assert_same([
                        "Runner request failed with HTTP 401. Check the runner connection and authorization.\n",
                    ], $errors);
                }
            } else {
                assert_same([], $errors);
                assert_same([
                    $case === 'no_work'
                        ? "No eligible queued work was returned for this runner.\n"
                        : 'Run '.$claim->runId.', attempt '.$claim->attemptId.': '.$outcome.".\n",
                ], $terminal);
            }

            assert_true(! str_contains(json_encode([$terminal, $errors]), 'SYNTHETIC_PRIVATE'));
        } finally {
            remove_directory($directory);
        }
    });
}

runner_test('setup keeps a maximum-size artifact manifest below the shared request throttle', function (): void {
    $directory = temporary_directory();
    $archive = tar_archive('file.txt', 'safe');
    $references = artifact_references($archive);
    $claim = fixture_claim([
        'source_artifacts' => array_fill(0, 20, $references['source']),
        'instruction_artifacts' => array_fill(0, 20, $references['instruction']),
    ]);
    $output = json_encode(['events' => [], 'artifacts' => [], 'result' => [
        'protocol_version' => '1.0',
        'run_id' => $claim->runId,
        'attempt_id' => $claim->attemptId,
        'fence' => $claim->fence,
        'summary' => 'done',
        'outcome' => 'incomplete',
        'findings' => [],
        'patch_artifact' => null,
        'tests' => [],
        'usage' => null,
    ]], JSON_THROW_ON_ERROR);
    $clock = new FakeClock;
    $client = new FakeControlPlane($claim, $archive, clock: $clock);
    $client->onDownload = function () use ($clock): void {
        $clock->time += 1;
    };
    $supervisor = new Supervisor(
        $client,
        new FakeSandbox(new FakeProcess(running: false, nativeOutput: $output)),
        new FakeNativeExecutableAdapter,
        new AttemptStateStore($directory.'/active.json'),
        new WorkspacePreparer($directory.'/workspaces'),
        clock: $clock,
        watchdog: new FakeWatchdog,
    );

    try {
        assert_same(true, $supervisor->runOnce());
        assert_true(
            40 + $client->heartbeatCount + 3 <= 60,
            'Setup consumed too much of the 60 request/minute budget.',
        );
    } finally {
        remove_directory($directory);
    }
});

runner_test('completion uses the server identifier returned for one uploaded patch', function (): void {
    $directory = temporary_directory();
    $archive = tar_archive('file.txt', 'safe');
    $references = artifact_references($archive);
    $claim = fixture_claim([
        'source_artifacts' => [$references['source']],
        'instruction_artifacts' => [$references['instruction']],
    ]);
    $patchBytes = 'normalized patch bytes';
    $patchHash = hash('sha256', $patchBytes);
    $output = json_encode(['events' => [], 'artifacts' => [[
        'kind' => 'patch',
        'bytes' => $patchBytes,
        'sha256' => $patchHash,
    ]], 'result' => [
        'protocol_version' => '1.0',
        'run_id' => $claim->runId,
        'attempt_id' => $claim->attemptId,
        'fence' => $claim->fence,
        'summary' => 'patch ready',
        'outcome' => 'changes_proposed',
        'findings' => [],
        'patch_artifact' => null,
        'tests' => [],
        'usage' => null,
    ]], JSON_THROW_ON_ERROR);
    $clock = new FakeClock;
    $client = new FakeControlPlane($claim, $archive, clock: $clock);
    $supervisor = new Supervisor(
        $client,
        new FakeSandbox(new FakeProcess(running: false, nativeOutput: $output)),
        new FakeNativeExecutableAdapter,
        new AttemptStateStore($directory.'/active.json'),
        new WorkspacePreparer($directory.'/workspaces'),
        clock: $clock,
        watchdog: new FakeWatchdog,
    );

    try {
        assert_same(true, $supervisor->runOnce());
        assert_same([
            'artifact_id' => '01k4w000000000000000000003',
            'sha256' => $patchHash,
        ], $client->completedResult['patch_artifact'] ?? null);
    } finally {
        remove_directory($directory);
    }
});

runner_test('multiple patch artifacts are rejected before upload or completion', function (): void {
    $directory = temporary_directory();
    $archive = tar_archive('file.txt', 'safe');
    $references = artifact_references($archive);
    $claim = fixture_claim([
        'source_artifacts' => [$references['source']],
        'instruction_artifacts' => [$references['instruction']],
    ]);
    $patch = [
        'kind' => 'patch',
        'bytes' => 'patch',
        'sha256' => hash('sha256', 'patch'),
    ];
    $output = json_encode(['events' => [], 'artifacts' => [$patch, $patch], 'result' => [
        'protocol_version' => '1.0',
        'run_id' => $claim->runId,
        'attempt_id' => $claim->attemptId,
        'fence' => $claim->fence,
        'summary' => 'ambiguous',
        'outcome' => 'changes_proposed',
        'findings' => [],
        'patch_artifact' => null,
        'tests' => [],
        'usage' => null,
    ]], JSON_THROW_ON_ERROR);

    $clock = new FakeClock;
    $client = new FakeControlPlane($claim, $archive, clock: $clock);
    $supervisor = new Supervisor(
        $client,
        new FakeSandbox(new FakeProcess(running: false, nativeOutput: $output)),
        new FakeNativeExecutableAdapter,
        new AttemptStateStore($directory.'/active.json'),
        new WorkspacePreparer($directory.'/workspaces'),
        clock: $clock,
        watchdog: new FakeWatchdog,
    );

    try {
        assert_throws(RuntimeException::class, fn () => $supervisor->runOnce());
        assert_same([], $client->uploaded);
        assert_true(! $client->completed);
    } finally {
        remove_directory($directory);
    }
});

runner_test('independent host watchdog removes a live sandbox after its supervisor is killed', function (): void {
    $image = getenv('SHIPMUNK_RUNNER_IMAGE');
    assert_true(is_string($image) && $image !== '', 'SHIPMUNK_RUNNER_IMAGE is required.');
    $directory = temporary_directory();
    $claim = new Claim(
        '01k4w000000000000000000001',
        '01'.bin2hex(random_bytes(12)),
        99,
        new DateTimeImmutable('+45 seconds'),
        new DateTimeImmutable('+60 seconds'),
        [
            'protocol_version' => '1.0',
            'fake_mode' => 'ignore_term',
            'source_artifacts' => [],
            'instruction_artifacts' => [],
        ],
    );
    $commands = new CommandRunner;
    $sandbox = new DockerSandbox($image, $commands);
    $process = $sandbox->create($claim, (new AgentInputSanitizer)->sanitize($claim->manifest), $directory);
    $state = new AttemptStateStore($directory.'/active.json');
    $state->save($claim, $process, $claim->leaseExpiresAt, $directory.'/workspace');
    try {
        $process->start();
    } catch (Throwable $exception) {
        try {
            $sandbox->reconcile($process->id());
        } finally {
            remove_directory($directory);
        }
        throw $exception;
    }
    $ready = $directory.'/watchdog-ready';
    $controller = pcntl_fork();

    if ($controller === 0) {
        try {
            $watchdogLease = (new HostWatchdog($commands))->arm(
                $process->id(),
                $claim->leaseExpiresAt,
                $claim->deadline,
            );
            file_put_contents($ready, 'ready');
            sleep(600);
            $watchdogLease->disarm();
        } catch (Throwable) {
            // The parent detects the missing readiness marker.
        }

        exit(1);
    }

    assert_true($controller > 0, 'Unable to fork watchdog test controller.');

    try {
        for ($attempt = 0; $attempt < 100 && ! is_file($ready); $attempt++) {
            usleep(50_000);
        }

        assert_true(is_file($ready), 'Independent watchdog did not arm.');
        $commands->mustRun(['kill', '-KILL', (string) $controller]);
        pcntl_waitpid($controller, $status);
        $removed = false;

        for ($attempt = 0; $attempt < 120; $attempt++) {
            if ($commands->run(['docker', 'inspect', $process->id()])->exitCode !== 0) {
                $removed = true;
                break;
            }

            usleep(50_000);
        }

        assert_true($removed, 'Independent watchdog left the sandbox process tree alive.');
        assert_true($state->load() !== null, 'Watchdog cleared state before restart acknowledgement.');
    } finally {
        $sandbox->reconcile($process->id());
        remove_directory($directory);
    }
});

runner_test('Docker sandbox enforces non-root isolation resource limits and no credential inheritance', function (): void {
    $image = getenv('SHIPMUNK_RUNNER_IMAGE');
    assert_true(is_string($image) && $image !== '', 'SHIPMUNK_RUNNER_IMAGE is required.');
    putenv('APP_KEY=host-secret');
    putenv('GITHUB_TOKEN=host-github-secret');
    putenv('SHIPMUNK_RUNNER_TOKEN=host-runner-secret');
    $directory = temporary_directory();
    file_put_contents($directory.'/fixture.txt', 'fixture');
    $bulk = fopen($directory.'/bulk.bin', 'x');
    assert_true(is_resource($bulk), 'Unable to prepare large workspace fixture.');
    $chunk = str_repeat('x', 1024 * 1024);
    for ($megabyte = 0; $megabyte < 65; $megabyte++) {
        assert_same(strlen($chunk), fwrite($bulk, $chunk), 'Unable to write large workspace fixture.');
    }
    fclose($bulk);
    $claim = fixture_claim([
        'fake_mode' => 'ignore_term',
        'fake_require_fixture' => true,
        'safe_marker' => 'copied',
        'supervisor' => ['credential_reference' => 'credential:secret'],
        'runner_token' => 'must-not-enter',
    ], '01'.bin2hex(random_bytes(12)));
    $commands = new CommandRunner;
    $sandbox = new DockerSandbox($image, $commands);
    $process = $sandbox->create($claim, (new AgentInputSanitizer)->sanitize($claim->manifest), $directory);

    try {
        $inspect = json_decode($commands->mustRun(['docker', 'inspect', $process->id()])->stdout, true, flags: JSON_THROW_ON_ERROR)[0];
        assert_same('65532:65532', $inspect['Config']['User']);
        assert_same(true, $inspect['HostConfig']['ReadonlyRootfs']);
        assert_same('none', $inspect['HostConfig']['NetworkMode']);
        assert_same(['ALL'], $inspect['HostConfig']['CapDrop']);
        assert_true(in_array('no-new-privileges:true', $inspect['HostConfig']['SecurityOpt'], true));
        assert_same(268_435_456, $inspect['HostConfig']['Memory']);
        assert_same(268_435_456, $inspect['HostConfig']['MemorySwap']);
        assert_same(64, $inspect['HostConfig']['PidsLimit']);
        assert_same(1_000_000_000, $inspect['HostConfig']['NanoCpus']);
        assert_same(null, $inspect['HostConfig']['Binds']);
        assert_same('json-file', $inspect['HostConfig']['LogConfig']['Type']);
        assert_same('false', $inspect['HostConfig']['LogConfig']['Config']['compress']);
        assert_same('1', $inspect['HostConfig']['LogConfig']['Config']['max-file']);
        assert_true(isset($inspect['HostConfig']['Tmpfs']['/workspace']));

        $checkpoints = 0;
        $process->start(function () use (&$checkpoints, $commands, $process): void {
            $checkpoints++;

            if ($checkpoints === 1) {
                assert_true($process->isRunning(), 'Sandbox bootstrap was not running before input copy.');
                assert_same('', $commands->mustRun(['docker', 'logs', $process->id()])->stdout);
            }
        });
        assert_true($checkpoints >= 5, 'Sandbox setup did not expose heartbeat checkpoints.');
        assert_same(
            (string) (65 * 1024 * 1024),
            trim($commands->mustRun(['docker', 'exec', $process->id(), 'sh', '-c', 'wc -c < /workspace/bulk.bin'])->stdout),
        );
        assert_same('65532', trim($commands->mustRun(['docker', 'exec', $process->id(), 'id', '-u'])->stdout));
        $observed = null;

        for ($attempt = 0; $attempt < 20; $attempt++) {
            $result = $commands->run(['docker', 'exec', $process->id(), 'cat', '/run/shipmunk/observed']);

            if ($result->exitCode === 0) {
                $observed = $result->stdout;
                break;
            }

            usleep(50_000);
        }

        assert_same('fixture:copied', $observed, 'Native process did not observe staged trusted inputs.');
        $agentInput = $commands->mustRun(['docker', 'exec', $process->id(), 'cat', '/run/shipmunk/agent-input.json'])->stdout;
        assert_true(! str_contains($agentInput, 'credential:secret'));
        assert_true(! str_contains($agentInput, 'must-not-enter'));
        $environment = $commands->mustRun(['docker', 'exec', $process->id(), 'env'])->stdout;
        assert_true(! str_contains($environment, 'host-secret'));
        assert_true(! str_contains($environment, 'host-github-secret'));
        assert_true(! str_contains($environment, 'host-runner-secret'));
        assert_true($commands->run(['docker', 'exec', $process->id(), 'touch', '/forbidden'])->exitCode !== 0);
        assert_same(0, $commands->run(['docker', 'exec', $process->id(), 'test', '!', '-e', '/var/run/docker.sock'])->exitCode);
        assert_true($commands->run([
            'docker', 'exec', $process->id(),
            'wget', '-T', '1', '-qO-', 'http://192.0.2.1',
        ])->exitCode !== 0);

        $top = $commands->mustRun(['docker', 'top', $process->id(), '-eo', 'pid,comm'])->stdout;
        assert_true(str_contains($top, 'sleep'));
        $process->stop();
        assert_true(! $process->isRunning(), 'Container process tree survived stop escalation.');
        assert_same(137, $process->exitCode(), 'TERM-ignoring process tree was not hard-killed.');
    } finally {
        $process->remove();
        remove_directory($directory);
        putenv('APP_KEY');
        putenv('GITHUB_TOKEN');
        putenv('SHIPMUNK_RUNNER_TOKEN');
    }
});

$failures = 0;

foreach ($tests as $name => $test) {
    try {
        $test();
        fwrite(STDOUT, "PASS {$name}\n");
    } catch (Throwable $exception) {
        $failures++;
        fwrite(STDERR, "FAIL {$name}\n  {$exception->getMessage()}\n");
    }
}

fwrite(STDOUT, sprintf("\n%d passed, %d failed\n", count($tests) - $failures, $failures));
exit($failures === 0 ? 0 : 1);
