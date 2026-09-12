<?php

declare(strict_types=1);

use Shipmunk\Runner\AttemptStateStore;
use Shipmunk\Runner\CommandRunner;
use Shipmunk\Runner\Drivers\AgentSessionStore;
use Shipmunk\Runner\Drivers\CodexSandbox;
use Shipmunk\Runner\Drivers\CodexWatchdog;
use Shipmunk\Runner\HttpControlPlaneClient;
use Shipmunk\Runner\HttpResponse;
use Shipmunk\Runner\HttpTransport;
use Shipmunk\Runner\NormalizedExecutionAdapter;
use Shipmunk\Runner\Profiles\NativeProfile;
use Shipmunk\Runner\Profiles\ProfileStore;
use Shipmunk\Runner\RunCommand;
use Shipmunk\Runner\Supervisor;
use Shipmunk\Runner\WorkspacePreparer;

require dirname(__DIR__).'/bootstrap.php';

function codex_supervisor_assert(
    bool $condition,
    string $message,
): void {
    if (! $condition) {
        throw new RuntimeException($message);
    }
}

function codex_supervisor_absent(string $name): void
{
    $commands = new CommandRunner;

    foreach ([$name, $name.'-repo', $name.'-diff'] as $container) {
        $inspection = $commands->run(['docker', 'inspect', $container]);
        codex_supervisor_assert(
            $inspection->exitCode !== 0 && str_contains(strtolower($inspection->stderr), 'no such'),
            'A native, repository or diff container survived stopped acknowledgement.',
        );
    }

    $volume = $commands->run(['docker', 'volume', 'inspect', $name.'-workspace']);
    codex_supervisor_assert(
        $volume->exitCode !== 0 && str_contains(strtolower($volume->stderr), 'no such volume'),
        'The repository volume survived stopped acknowledgement.',
    );
}

function codex_supervisor_remove(string $root): void
{
    $iterator = new RecursiveIteratorIterator(
        new RecursiveDirectoryIterator($root, FilesystemIterator::SKIP_DOTS),
        RecursiveIteratorIterator::CHILD_FIRST,
    );

    foreach ($iterator as $entry) {
        if ($entry->isDir() && ! $entry->isLink()) {
            rmdir($entry->getPathname());
        } else {
            unlink($entry->getPathname());
        }
    }

    rmdir($root);
}

/**
 * The HTTP transport is simulated; its real client still checks envelopes, hashes and fences.
 */
final class CodexSupervisorHttp implements HttpTransport
{
    public array $downloads = [];

    public array $events = [];

    public array $uploads = [];

    public array $completions = [];

    public bool $stopped = false;

    public bool $cancelledRunningChildren = false;

    public bool $sessionWriteBlocked = false;

    public function __construct(
        private readonly array $manifest,
        private readonly array $inputs,
        private readonly ProfileStore $profile,
        private readonly string $workspace,
        private readonly string $sandboxName,
        private readonly bool $cancel,
        private readonly ?string $blockSessionWrite = null,
    ) {}

    public function request(
        string $method,
        string $url,
        array $headers,
        string $body,
        int $timeoutSeconds,
        int $maxResponseBytes,
    ): HttpResponse {
        codex_supervisor_assert(($headers['Authorization'] ?? null) === 'Bearer SYNTHETIC_RUNNER_TOKEN', 'Missing runner authentication.');
        $path = parse_url($url, PHP_URL_PATH);
        $attemptPath = '/runner/v1/attempts/'.$this->manifest['attempt_id'];

        if ($path === '/runner/v1/claims') {
            codex_supervisor_assert($method === 'POST', 'Unexpected claim method.');

            return $this->json($this->manifest);
        }

        codex_supervisor_assert(str_starts_with($path, $attemptPath.'/'), 'Request escaped the assigned attempt.');

        if ($method === 'GET') {
            $artifact = basename($path);
            $bytes = $this->inputs[$artifact] ?? throw new RuntimeException('Unknown input artifact.');
            $this->downloads[] = $artifact;

            return new HttpResponse(200, [
                'content-length' => (string) strlen($bytes),
                'x-artifact-sha256' => hash('sha256', $bytes),
            ], $bytes);
        }

        if ($path === $attemptPath.'/artifacts') {
            codex_supervisor_assert($headers['X-Attempt-Fence'] === '1', 'Artifact lost the active fence.');
            codex_supervisor_assert($headers['X-Artifact-Kind'] === 'patch', 'Unexpected output artifact.');
            codex_supervisor_assert(hash('sha256', $body) === $headers['X-Artifact-SHA256'], 'Artifact envelope hash mismatch.');
            $this->uploads[] = [
                'bytes' => $body,
                'sha256' => $headers['X-Artifact-SHA256'],
            ];

            return $this->json([
                'id' => '01k4w000000000000000000009',
            ]);
        }

        $payload = json_decode($body, true, flags: JSON_THROW_ON_ERROR);

        if ($path === $attemptPath.'/events') {
            foreach ($payload as $event) {
                $this->assertFence($event);
                $this->events[] = $event;
            }

            return new HttpResponse(204, [], '');
        }

        $this->assertFence($payload);

        if ($path === $attemptPath.'/completion') {
            codex_supervisor_assert($this->profile->read('execution') !== null, 'Completion escaped durable profile exclusion.');
            $this->completions[] = $payload;

            return new HttpResponse(204, [], '');
        }

        codex_supervisor_assert($path === $attemptPath.'/heartbeat', 'Unexpected attempt endpoint.');

        if (($payload['stopped'] ?? false) === true) {
            codex_supervisor_absent($this->sandboxName);
            codex_supervisor_assert(! is_dir($this->workspace), 'Stopped acknowledgement preceded workspace cleanup.');
            codex_supervisor_assert($this->profile->read('execution') === null, 'Stopped acknowledgement preceded journal release.');
            $this->profile->validateHome();
            $this->stopped = true;

            return new HttpResponse(204, [], '');
        }

        if ($this->cancel && is_file($this->profile->home().'/cancellation-ready')) {
            $children = (new CommandRunner)->mustRun(['docker', 'top', $this->sandboxName.'-repo', '-eo', 'pid,args']);
            codex_supervisor_assert(str_contains($children->stdout, 'sleep 300'), 'Cancellation did not interrupt a live repository child.');
            $this->cancelledRunningChildren = true;
        }

        if ($this->blockSessionWrite !== null && is_dir($this->blockSessionWrite)) {
            // Session lookup has completed; make the later optional write fail its protection check.
            chmod($this->blockSessionWrite, 0755);
            $this->sessionWriteBlocked = true;
        }

        return $this->json([
            'protocol_version' => '1.0',
            'attempt_id' => $this->manifest['attempt_id'],
            'fence' => 1,
            'lease_expires_at' => (new DateTimeImmutable('+45 seconds'))->format(DATE_ATOM),
            'stop_requested' => $this->cancelledRunningChildren,
        ]);
    }

    private function assertFence(array $payload): void
    {
        codex_supervisor_assert(($payload['protocol_version'] ?? null) === '1.0', 'Protocol version missing.');
        codex_supervisor_assert(($payload['attempt_id'] ?? null) === $this->manifest['attempt_id'], 'Attempt identity mismatch.');
        codex_supervisor_assert(($payload['fence'] ?? null) === 1, 'Attempt fence mismatch.');
    }

    private function json(array $data): HttpResponse
    {
        return new HttpResponse(200, [], json_encode([
            'data' => $data,
        ], JSON_THROW_ON_ERROR));
    }
}

function codex_supervisor_scenario(
    string $image,
    bool $cancel,
    bool $failSessionWrite = false,
    bool $failPreflight = false,
): void {
    $root = realpath(sys_get_temp_dir()).'/shipmunk-codex-supervisor-'.bin2hex(random_bytes(8));
    mkdir($root, 0700);
    $repository = $root.'/repository';
    mkdir($repository, 0700);
    file_put_contents($repository.'/README.md', "original\n");
    file_put_contents($repository.'/AGENTS.md', "UNTRUSTED_NATIVE_OVERRIDE\n");
    $commands = new CommandRunner;
    $commands->mustRun(['git', '-C', $repository, 'init', '-q']);
    $commands->mustRun(['git', '-C', $repository, 'add', '--all']);
    $commands->mustRun(['git', '-C', $repository, '-c', 'core.hooksPath=/dev/null', '-c', 'user.name=fixture', '-c', 'user.email=fixture@example.test', 'commit', '-qm', 'fixture']);
    $sha = trim($commands->mustRun(['git', '-C', $repository, 'rev-parse', 'HEAD'])->stdout);
    $archive = gzencode($commands->mustRun(['git', '-C', $repository, 'archive', '--format=tar', '--prefix=fixture-'.$sha.'/', 'HEAD'])->stdout);
    $instructions = 'APPROVED_BUNDLE';
    $trustedAgents = 'APPROVED_AGENT';
    $configurationHash = str_repeat('c', 64);
    $bundle = json_encode([
        'version' => 1,
        'trusted_revision' => $sha,
        'effective_configuration_sha256' => $configurationHash,
        'effective_instructions' => [
            'contents' => $instructions,
            'sha256' => hash('sha256', $instructions),
        ],
        'trusted_files' => [
            'AGENTS.md' => [
                'path' => 'AGENTS.md',
                'contents' => $trustedAgents,
                'sha256' => hash('sha256', $trustedAgents),
            ],
        ],
    ], JSON_THROW_ON_ERROR);
    $attempt = '01'.bin2hex(random_bytes(12));
    $profileId = '01kkkkkkkkkkkkkkkkkkkkkkkk';
    $manifest = [
        'protocol_version' => '1.0',
        'run_id' => '01k4w000000000000000000001',
        'attempt_id' => $attempt,
        'fence' => 1,
        'deadline' => (new DateTimeImmutable('+5 minutes'))->format(DATE_ATOM),
        'profile_id' => $profileId,
        'agent' => 'codex',
        'runtime_version' => NativeProfile::VERSIONS['codex'],
        'kind' => 'implementation',
        'base_sha' => $sha,
        'head_sha' => $sha,
        'task_context' => $cancel ? 'SCENARIO:cancel' : 'SCENARIO:success',
        'effective_config' => [
            'model' => 'synthetic-model',
            'max_turns' => 5,
            'instructions' => $instructions,
            'instructions_sha256' => hash('sha256', $instructions),
            'trusted_revision' => $sha,
            'effective_configuration_sha256' => $configurationHash,
        ],
        'supervisor' => [
            'credential_reference' => 'credential:synthetic-profile',
        ],
        'source_artifacts' => [[
            'artifact_id' => '01k4w000000000000000000003',
            'sha256' => hash('sha256', $archive),
        ]],
        'instruction_artifacts' => [[
            'artifact_id' => '01k4w000000000000000000004',
            'sha256' => hash('sha256', $bundle),
        ]],
    ];
    mkdir($root.'/profiles', 0700);
    $profile = new ProfileStore($root.'/profiles', $profileId);
    NativeProfile::initializeHome($profile->createHome());
    file_put_contents($profile->home().'/synthetic-secret', 'SYNTHETIC_PROFILE_SECRET');
    chmod($profile->home().'/synthetic-secret', 0600);
    if ($failPreflight) {
        file_put_contents($profile->home().'/synthetic-preflight-failure', 'fixture');
        chmod($profile->home().'/synthetic-preflight-failure', 0600);
    }
    $profile->write('active', [
        'profile_id' => $profileId,
        'credential_reference' => 'credential:synthetic-profile',
        'agent' => 'codex',
        'auth_mode' => 'subscription',
        'runtime_version' => NativeProfile::VERSIONS['codex'],
    ]);
    $name = 'shipmunk-codex-'.$attempt.'-1';
    $workspace = $root.'/workspaces/'.$attempt.'-1';
    $http = new CodexSupervisorHttp($manifest, [
        '01k4w000000000000000000003' => $archive,
        '01k4w000000000000000000004' => $bundle,
    ], $profile, $workspace, $name, $cancel, $failSessionWrite ? $root.'/sessions' : null);
    $sandbox = new CodexSandbox($root.'/profiles', $image, $image, new AgentSessionStore($root.'/sessions'));
    $state = new AttemptStateStore($root.'/active-attempt.json');
    $supervisor = new Supervisor(
        new HttpControlPlaneClient('https://control-plane.example.test', 'SYNTHETIC_RUNNER_TOKEN', $http),
        $sandbox,
        new NormalizedExecutionAdapter,
        $state,
        new WorkspacePreparer($root.'/workspaces'),
        watchdog: new CodexWatchdog,
    );

    try {
        $failure = null;
        $terminal = [];
        $errors = [];
        $commandExit = null;

        try {
            if ($failPreflight) {
                $commandExit = (new RunCommand(
                    $supervisor,
                    static function (string $message) use (&$terminal, $http, $state): void {
                        codex_supervisor_assert(
                            $http->stopped && $state->load() === null,
                            'Terminal completion preceded acknowledged cleanup.',
                        );
                        $terminal[] = $message;
                    },
                    static function (string $message) use (&$errors): void {
                        $errors[] = $message;
                    },
                ))->run(true);
            } else {
                $supervisor->runOnce();
            }
        } catch (RuntimeException $exception) {
            $failure = $exception;
        }

        codex_supervisor_assert($http->stopped, 'Supervisor did not acknowledge confirmed cleanup.');
        codex_supervisor_assert($state->load() === null, 'Durable attempt state survived confirmed cleanup.');
        codex_supervisor_assert($http->downloads === [
            '01k4w000000000000000000003',
            '01k4w000000000000000000004',
        ], 'Source and trusted instructions did not traverse artifact download.');
        codex_supervisor_assert(file_get_contents($repository.'/README.md') === "original\n", 'Host source was modified.');
        codex_supervisor_assert(file_get_contents($profile->home().'/synthetic-secret') === 'SYNTHETIC_PROFILE_SECRET', 'Assigned credentials were damaged.');

        if ($cancel) {
            codex_supervisor_assert($failure?->getMessage() === 'Control plane requested execution stop.', 'Cancellation failed through an unexpected path: '.($failure?->getMessage() ?? 'no exception'));
            codex_supervisor_assert($http->cancelledRunningChildren, 'Cancellation did not reach running repository code.');
            codex_supervisor_assert($http->completions === [] && $http->uploads === [], 'Cancelled execution published a result or patch.');

            return;
        }

        if ($failPreflight) {
            $summary = 'Native runtime execution failed. Stage: authenticated preflight.'
                .' Reason: process_error. Native exit code: 17.';
            codex_supervisor_assert(
                $failure === null && $commandExit === 1,
                'Incomplete native work did not produce a failing terminal exit.',
            );
            codex_supervisor_assert($terminal === [
                'Run '.$manifest['run_id'].', attempt '.$attempt.": incomplete.\n",
            ], 'Terminal output omitted or duplicated the assigned run result.');
            codex_supervisor_assert($errors === [], 'A reported native failure was replaced by a generic command failure.');
            codex_supervisor_assert(
                count($http->completions) === 1 && $http->uploads === [],
                'Preflight failure published work or omitted completion.',
            );
            codex_supervisor_assert(
                $http->completions[0]['summary'] === $summary,
                'Native failure stage or exit code was lost.',
            );
            codex_supervisor_assert(
                $http->completions[0]['outcome'] === 'incomplete',
                'Native preflight failure became successful.',
            );
            codex_supervisor_assert(
                array_column($http->events, 'type') === ['progress'],
                'Sanitized failure diagnostic was not published as progress.',
            );
            codex_supervisor_assert(
                $http->events[0]['payload'] === ['message' => $summary],
                'Diagnostic event differs from the sanitized result.',
            );
            $outbound = json_encode([$http->events, $http->completions, $terminal, $errors], JSON_THROW_ON_ERROR);
            codex_supervisor_assert(
                ! str_contains($outbound, 'SYNTHETIC_PRIVATE'),
                'Raw native diagnostics escaped the boundary.',
            );
            codex_supervisor_assert(
                ! str_contains($outbound, 'SYNTHETIC_PROFILE_SECRET'),
                'Profile credentials escaped the boundary.',
            );
            codex_supervisor_assert(
                ! str_contains($outbound, 'SYNTHETIC_RUNNER_TOKEN'),
                'Runner credentials escaped the boundary.',
            );

            return;
        }

        codex_supervisor_assert($failure === null, 'Successful execution raised a supervisor failure.');
        codex_supervisor_assert(count($http->completions) === 1 && count($http->uploads) === 1, 'Successful execution omitted the patch or completion.');
        if ($failSessionWrite) {
            codex_supervisor_assert($http->sessionWriteBlocked, 'Session write failure was not exercised.');
            codex_supervisor_assert(! file_exists($root.'/sessions/'.$manifest['run_id'].'.json'), 'Failed session write persisted a record.');
        } else {
            codex_supervisor_assert(is_file($root.'/sessions/'.$manifest['run_id'].'.json'), 'Successful execution omitted its resume record.');
        }

        $result = $http->completions[0];
        codex_supervisor_assert($result['outcome'] === 'changes_proposed', 'Native result was not normalized successfully.');
        $artifact = json_decode($http->uploads[0]['bytes'], true, flags: JSON_THROW_ON_ERROR);
        codex_supervisor_assert($artifact['protocol_version'] === '1.0' && $artifact['base_sha'] === $sha, 'Patch envelope does not match the original snapshot.');
        codex_supervisor_assert(hash('sha256', $artifact['patch']) === $artifact['sha256'], 'Inner patch hash mismatch.');
        codex_supervisor_assert(str_contains($artifact['patch'], '+changed') && str_contains($artifact['patch'], '+added'), 'Repository commit tampering hid file edits.');
        codex_supervisor_assert(array_column($artifact['changed_files'], 'path') === ['README.md', 'added.txt'], 'Trusted changed-file metadata omitted edits.');
        codex_supervisor_assert($result['patch_artifact'] === [
            'artifact_id' => '01k4w000000000000000000009',
            'sha256' => $http->uploads[0]['sha256'],
        ], 'Completion did not use the uploaded patch identifier and hash.');
        codex_supervisor_assert(array_column($http->events, 'type') === ['progress', 'tool_started', 'tool_finished'], 'Native progress and tool events were not normalized.');
        codex_supervisor_assert(array_column($http->events, 'sequence') === [1, 2, 3], 'Event sequence is discontinuous.');
        $outbound = json_encode([$http->events, $http->uploads, $http->completions], JSON_THROW_ON_ERROR);
        codex_supervisor_assert(! str_contains($outbound, 'SYNTHETIC_PROFILE_SECRET'), 'Native credentials leaked through publication.');
        codex_supervisor_assert(! str_contains($outbound, 'SYNTHETIC_RUNNER_TOKEN'), 'Runner credentials leaked through publication.');
    } finally {
        $sandbox->reconcile($name);
        codex_supervisor_absent($name);
        codex_supervisor_remove($root);
    }
}

$image = getenv('SHIPMUNK_CODEX_SUPERVISOR_IMAGE') ?: 'shipmunk-codex-supervisor-test:local';

codex_supervisor_scenario($image, false, failPreflight: true);
fwrite(STDOUT, "PASS native preflight failure publishes sanitized stage and exit code before failing terminal feedback\n");

foreach ([false, true] as $cancel) {
    codex_supervisor_scenario($image, $cancel);
    fwrite(STDOUT, $cancel
        ? "PASS real-container supervisor cancellation stops native and repository children before acknowledgement\n"
        : "PASS real-container supervisor downloads trusted inputs runs MCP and publishes a verified patch envelope\n");
}

// Each fork must close channels inherited from earlier watchdog siblings. Otherwise the
// first child cannot observe parent death until a later child's 45-second lease expires.
codex_supervisor_scenario($image, false, true);
fwrite(STDOUT, "PASS completed execution publishes its patch and result despite optional session write failure\n");

$root = realpath(sys_get_temp_dir()).'/shipmunk-codex-watchdog-'.bin2hex(random_bytes(8));
mkdir($root, 0700);
$name = 'shipmunk-codex-01'.bin2hex(random_bytes(12)).'-1';
$commands = new CommandRunner;
$process = null;

try {
    foreach ([$name, $name.'-repo', $name.'-diff'] as $container) {
        $commands->mustRun([
            'docker', 'run', '--detach', '--name', $container,
            '--network', 'none', '--entrypoint', '/bin/sleep', $image, '300',
        ]);
    }

    $process = proc_open([
        PHP_BINARY,
        __DIR__.'/fixtures/codex-supervisor/watchdog-parent.php',
        $name,
        $root.'/ready',
    ], [
        0 => ['file', '/dev/null', 'r'],
        1 => ['file', '/dev/null', 'w'],
        2 => ['file', '/dev/null', 'w'],
    ], $pipes);
    codex_supervisor_assert(is_resource($process), 'Unable to start isolated watchdog parent.');
    $readyDeadline = hrtime(true) + 5_000_000_000;

    while (! is_file($root.'/ready')) {
        codex_supervisor_assert(hrtime(true) < $readyDeadline, 'Composite watchdog did not arm.');
        usleep(25_000);
    }

    proc_terminate($process, 9);
    proc_close($process);
    $process = null;
    $cleanupDeadline = hrtime(true) + 10_000_000_000;
    $remaining = [$name, $name.'-repo', $name.'-diff'];

    do {
        $remaining = array_values(array_filter($remaining, function (string $container) use ($commands): bool {
            return $commands->run(['docker', 'inspect', $container])->exitCode === 0;
        }));

        if ($remaining !== []) {
            usleep(50_000);
        }
    } while ($remaining !== [] && hrtime(true) < $cleanupDeadline);

    codex_supervisor_assert($remaining === [], 'A sibling watchdog retained the dead parent channel until lease expiry.');
    fwrite(STDOUT, "PASS composite watchdog detects parent death and removes all three siblings before lease expiry\n");
} finally {
    if (is_resource($process)) {
        proc_terminate($process, 9);
        proc_close($process);
    }

    foreach ([$name, $name.'-repo', $name.'-diff'] as $container) {
        $commands->run(['docker', 'rm', '--force', $container]);
    }

    codex_supervisor_remove($root);
}

fwrite(STDOUT, "5 Codex supervisor scenarios passed. Native CLI and HTTP responses are synthetic; "
    ."Docker, MCP, filesystem and supervisor are real.\n");
