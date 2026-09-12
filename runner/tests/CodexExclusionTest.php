<?php

declare(strict_types=1);

use Shipmunk\Runner\AttemptStateStore;
use Shipmunk\Runner\Claim;
use Shipmunk\Runner\CommandResult;
use Shipmunk\Runner\ControlPlaneClient;
use Shipmunk\Runner\Drivers\AgentSessionStore;
use Shipmunk\Runner\Drivers\CodexSandbox;
use Shipmunk\Runner\Heartbeat;
use Shipmunk\Runner\NormalizedExecutionAdapter;
use Shipmunk\Runner\Profiles\NativeProfile;
use Shipmunk\Runner\Profiles\ProfileControlPlane;
use Shipmunk\Runner\Profiles\ProfileExecutionGate;
use Shipmunk\Runner\Profiles\ProfileLifecycle;
use Shipmunk\Runner\Profiles\ProfileRuntime;
use Shipmunk\Runner\Profiles\ProfileStore;
use Shipmunk\Runner\Supervisor;
use Shipmunk\Runner\WorkspacePreparer;

require dirname(__DIR__).'/bootstrap.php';

function exclusion_assert(bool $condition): void
{
    if (! $condition) {
        throw new RuntimeException('Execution exclusion assertion failed.');
    }
}

function exclusion_rejects(
    Closure $operation,
    string $expectedMessage,
): void {
    try {
        $operation();
    } catch (RuntimeException $exception) {
        if ($exception->getMessage() !== $expectedMessage) {
            throw new RuntimeException('Unexpected rejection: '.$exception->getMessage(), previous: $exception);
        }

        return;
    }

    throw new RuntimeException('Expected execution exclusion rejection.');
}

function exclusion_claim(int $fence = 1): Claim
{
    return new Claim(
        '01k4w000000000000000000001',
        '01k4w000000000000000000002',
        $fence,
        new DateTimeImmutable('2099-01-01T00:00:45Z'),
        new DateTimeImmutable('2099-01-01T00:30:00Z'),
        [
            'protocol_version' => '1.0',
            'profile_id' => '01kkkkkkkkkkkkkkkkkkkkkkkk',
            'agent' => 'codex',
            'runtime_version' => NativeProfile::VERSIONS['codex'],
            'supervisor' => [
                'credential_reference' => 'credential:fixture',
            ],
        ],
    );
}

/**
 * Real protected storage and flock; process cleanup is the simulated external boundary.
 */
function exclusion_fixture(Closure $cleanup): array
{
    $root = realpath(sys_get_temp_dir()).'/shipmunk-exclusion-'.bin2hex(random_bytes(8));
    mkdir($root, 0700);
    $claim = exclusion_claim();
    $store = new ProfileStore($root, $claim->manifest['profile_id']);
    $store->createHome();
    $store->write('active', [
        'profile_id' => $claim->manifest['profile_id'],
        'credential_reference' => 'credential:fixture',
        'agent' => 'codex',
        'auth_mode' => 'subscription',
        'runtime_version' => NativeProfile::VERSIONS['codex'],
    ]);
    $sandbox = new CodexSandbox(
        $root,
        'simulated-native-image',
        'simulated-repository-image',
        new AgentSessionStore($root.'/sessions'),
        $cleanup,
    );

    return [$root, $claim, $store, $sandbox];
}

function exclusion_remove(string $root): void
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

final class ExclusionProfileApi implements ProfileControlPlane
{
    public int $requests = 0;

    public function request(
        string $profileId,
        string $suffix,
        array $payload,
    ): array {
        $this->requests++;

        throw new RuntimeException('Unexpected profile control-plane side effect.');
    }
}

final class ExclusionProfileRuntime implements ProfileRuntime
{
    public int $calls = 0;

    public function start(
        string $sandbox,
        string $home,
    ): void {
        $this->calls++;
    }

    public function run(
        string $sandbox,
        string $agent,
        string $command,
        Closure $checkpoint,
    ): CommandResult {
        $this->calls++;

        throw new RuntimeException('Unexpected native profile command.');
    }

    public function stop(string $sandbox): void
    {
        $this->calls++;
    }
}

final class ExclusionControlPlane implements ControlPlaneClient
{
    public int $acknowledgements = 0;

    public function __construct(
        private readonly Closure $beforeAcknowledgement,
        private ?Claim $nextClaim = null,
    ) {}

    public function claim(): ?Claim
    {
        $claim = $this->nextClaim;
        $this->nextClaim = null;

        return $claim;
    }

    public function heartbeat(Claim $claim): Heartbeat
    {
        return new Heartbeat(false, $claim->leaseExpiresAt);
    }

    public function acknowledgeStopped(Claim $claim): void
    {
        ($this->beforeAcknowledgement)($claim);
        $this->acknowledgements++;
    }

    public function sendEvents(
        Claim $claim,
        array $events,
    ): void {
        throw new RuntimeException('Unexpected events.');
    }

    public function downloadArtifact(
        Claim $claim,
        string $artifactId,
        string $sha256,
    ): string {
        throw new RuntimeException('Unexpected download.');
    }

    public function uploadArtifact(
        Claim $claim,
        string $kind,
        string $bytes,
        string $sha256,
    ): string {
        throw new RuntimeException('Unexpected upload.');
    }

    public function complete(
        Claim $claim,
        array $result,
    ): void {
        throw new RuntimeException('Unexpected completion.');
    }
}

$tests = [];

$tests['uncertain cleanup releases flock but quarantines execution and every credential operation'] = function (): void {
    [$root, $claim, $store, $sandbox] = exclusion_fixture(static function (): void {});

    try {
        exclusion_rejects(fn () => $sandbox->withProfile($claim, static function (): void {}, function () use ($store): void {
            exclusion_assert($store->read('execution') !== null);

            throw new RuntimeException('Simulated cleanup uncertainty.');
        }), 'Simulated cleanup uncertainty.');

        $independentStore = new ProfileStore($root, $claim->manifest['profile_id']);
        $independentStore->exclusively(static function (): void {});
        exclusion_assert($independentStore->read('execution')['fence'] === 1);

        $authorized = false;
        exclusion_rejects(fn () => (new ProfileExecutionGate)->execute(
            $independentStore,
            $store->read('active'),
            function () use (&$authorized): void {
                $authorized = true;
            },
            static function (): void {
                throw new LogicException('Quarantined native execution was entered.');
            },
        ), 'Profile execution requires stopped recovery.');
        exclusion_assert(! $authorized);

        $api = new ExclusionProfileApi;
        $runtime = new ExclusionProfileRuntime;
        $lifecycle = new ProfileLifecycle($api, $runtime);

        foreach (['login', 'probe', 'disconnect'] as $operation) {
            exclusion_rejects(fn () => $lifecycle->operate(
                $independentStore,
                $claim->manifest['profile_id'],
                $operation,
                '01mmmmmmmmmmmmmmmmmmmmmmmm',
            ), 'Profile execution requires stopped recovery.');
        }

        exclusion_assert($api->requests === 0);
        exclusion_assert($runtime->calls === 0);
        exclusion_assert($store->read('pending') === null);
        exclusion_assert($store->read('active') !== null);
    } finally {
        exclusion_remove($root);
    }
};

$tests['restart retains uncertain execution then confirms cleanup normalizes home and acknowledges stopped'] = function (): void {
    $failCleanup = true;
    $cleanups = [];
    [$root, $claim, $store, $sandbox] = exclusion_fixture(function (string $name) use (&$failCleanup, &$cleanups): void {
        $cleanups[] = $name;

        if ($failCleanup) {
            throw new RuntimeException('Simulated Docker absence uncertainty.');
        }
    });

    try {
        $state = new AttemptStateStore($root.'/active-attempt.json');
        $workspaces = new WorkspacePreparer($root.'/workspaces');
        $workspace = $workspaces->path($claim);
        mkdir($workspace, 0700, true);
        $state->save($claim, null, $claim->leaseExpiresAt, $workspace);

        exclusion_rejects(fn () => $sandbox->withProfile($claim, static function (): void {}, function () use ($store): void {
            file_put_contents($store->home().'/native-metadata', 'simulated');
            chmod($store->home().'/native-metadata', 0644);

            throw new RuntimeException('Simulated crash before sandbox id persisted.');
        }), 'Simulated crash before sandbox id persisted.');

        $client = new ExclusionControlPlane(function (Claim $recovered) use ($claim, $store, $workspace): void {
            exclusion_assert($recovered->manifest['profile_id'] === $claim->manifest['profile_id']);
            exclusion_assert($store->read('execution') === null);
            exclusion_assert(! is_dir($workspace));
            ProfileStore::protect($store->home().'/native-metadata');
        });
        $supervisor = new Supervisor($client, $sandbox, new NormalizedExecutionAdapter, $state, $workspaces);

        exclusion_rejects(fn () => $supervisor->reconcileAfterRestart(), 'Simulated Docker absence uncertainty.');
        exclusion_assert($state->load() !== null);
        exclusion_assert($store->read('execution') !== null);
        exclusion_assert($client->acknowledgements === 0);

        $failCleanup = false;
        $supervisor->reconcileAfterRestart();

        exclusion_assert($cleanups === [
            'shipmunk-codex-01k4w000000000000000000002-1',
            'shipmunk-codex-01k4w000000000000000000002-1',
        ]);
        exclusion_assert($client->acknowledgements === 1);
        exclusion_assert($state->load() === null);

        $sandbox->withProfile($claim, static function (): void {}, fn () => $sandbox->releaseProfileExecution($claim));
        exclusion_assert($store->read('execution') === null);
    } finally {
        exclusion_remove($root);
    }
};

$tests['restart refuses another attempt reservation before cleanup and never clears its journal'] = function (): void {
    $cleanups = 0;
    [$root, $claim, $store, $sandbox] = exclusion_fixture(function () use (&$cleanups): void {
        $cleanups++;
    });

    try {
        $newer = exclusion_claim(2);
        $store->exclusively(fn () => $store->reserveExecution($newer));
        $marker = $store->read('execution');

        exclusion_rejects(fn () => $sandbox->reconcileProfileExecution($claim, null), 'Profile execution belongs to another attempt.');
        exclusion_rejects(fn () => $sandbox->reconcileProfileExecution($newer, 'shipmunk-codex-01k4w000000000000000000002-1'), 'Native restart sandbox does not match the attempt.');

        exclusion_assert($store->read('execution') === $marker);
        exclusion_assert($cleanups === 0);

        $sandbox->reconcileProfileExecution($newer, null);
        exclusion_assert($cleanups === 1);
        exclusion_assert($store->read('execution') === null);
    } finally {
        exclusion_remove($root);
    }
};

$tests['unsafe post-stop home keeps durable quarantine despite confirmed process cleanup'] = function (): void {
    [$root, $claim, $store, $sandbox] = exclusion_fixture(static function (): void {});

    try {
        $store->exclusively(fn () => $store->reserveExecution($claim));
        symlink('/simulated-outside-profile', $store->home().'/unsafe');

        exclusion_rejects(fn () => $sandbox->reconcileProfileExecution($claim, null), 'Unsafe native-generated profile entry.');
        exclusion_assert($store->read('execution') !== null);

        unlink($store->home().'/unsafe');
        $sandbox->reconcileProfileExecution($claim, null);
        exclusion_assert($store->read('execution') === null);
    } finally {
        exclusion_remove($root);
    }
};

$tests['rejected authorization leaves no reservation and old state remains readable without profile identity'] = function (): void {
    [$root, $claim, $store, $sandbox] = exclusion_fixture(static function (): void {});

    try {
        exclusion_rejects(fn () => $sandbox->withProfile($claim, static function (): void {
            throw new RuntimeException('Simulated stale authorization.');
        }, static function (): void {
            throw new LogicException('Unauthorized work was entered.');
        }), 'Simulated stale authorization.');
        exclusion_assert($store->read('execution') === null);

        $path = $root.'/active-attempt.json';
        $state = new AttemptStateStore($path);
        $state->save($claim, null, $claim->leaseExpiresAt, $root.'/workspaces/old');
        $old = json_decode(file_get_contents($path), true, flags: JSON_THROW_ON_ERROR);
        unset($old['profile_id']);
        file_put_contents($path, json_encode($old, JSON_THROW_ON_ERROR));

        exclusion_assert($state->load()['profile_id'] === null);
        $legacy = new Claim($claim->runId, $claim->attemptId, 1, $claim->leaseExpiresAt, $claim->deadline, [
            'protocol_version' => '1.0',
        ]);
        exclusion_rejects(fn () => $sandbox->reconcileProfileExecution($legacy, null), 'Native restart recovery requires a recorded profile.');
    } finally {
        exclusion_remove($root);
    }
};

$tests['pre-container preparation failure releases reservation before acknowledging the stopped attempt'] = function (): void {
    [$root, $claim, $store, $sandbox] = exclusion_fixture(static function (): void {});

    try {
        $state = new AttemptStateStore($root.'/active-attempt.json');
        $workspaces = new WorkspacePreparer($root.'/workspaces');
        $client = new ExclusionControlPlane(function () use ($store): void {
            exclusion_assert($store->read('execution') === null);
        }, $claim);
        $supervisor = new Supervisor($client, $sandbox, new NormalizedExecutionAdapter, $state, $workspaces);

        // The claim deliberately omits source references; failure follows profile reservation.
        exclusion_rejects(fn () => $supervisor->runOnce(), 'Claim source_artifacts is invalid.');

        exclusion_assert($client->acknowledgements === 1);
        exclusion_assert($state->load() === null);
        exclusion_assert($store->read('execution') === null);
    } finally {
        exclusion_remove($root);
    }
};

$tests['failure to enter a busy profile preserves durable attempt recovery and leaves its owner untouched'] = function (): void {
    $cleanups = 0;
    [$root, $claim, $store, $sandbox] = exclusion_fixture(function () use (&$cleanups): void {
        $cleanups++;
    });

    try {
        $state = new AttemptStateStore($root.'/active-attempt.json');
        $client = new ExclusionControlPlane(static function (): void {}, $claim);
        $supervisor = new Supervisor(
            $client,
            $sandbox,
            new NormalizedExecutionAdapter,
            $state,
            new WorkspacePreparer($root.'/workspaces'),
        );

        $store->exclusively(fn () => exclusion_rejects(fn () => $supervisor->runOnce(), 'Profile is already in use.'));

        exclusion_assert($client->acknowledgements === 0);
        exclusion_assert($state->load() !== null);
        exclusion_assert($store->read('execution') === null);
        exclusion_assert($cleanups === 0);

        $supervisor->reconcileAfterRestart();
        exclusion_assert($client->acknowledgements === 1);
        exclusion_assert($state->load() === null);
        exclusion_assert($cleanups === 1);
    } finally {
        exclusion_remove($root);
    }
};

$tests['malformed profile identities are rejected without persisting an unrecoverable attempt'] = function (): void {
    $cleanups = 0;
    [$root, $claim, $store, $sandbox] = exclusion_fixture(function () use (&$cleanups): void {
        $cleanups++;
    });

    try {
        foreach ([null, '', '../invalid', [], 42] as $profileId) {
            $manifest = $claim->manifest;
            $manifest['profile_id'] = $profileId;
            $invalid = new Claim(
                $claim->runId,
                $claim->attemptId,
                $claim->fence,
                $claim->leaseExpiresAt,
                $claim->deadline,
                $manifest,
            );
            $state = new AttemptStateStore($root.'/active-attempt.json');
            $client = new ExclusionControlPlane(static function (): void {}, $invalid);
            $supervisor = new Supervisor(
                $client,
                $sandbox,
                new NormalizedExecutionAdapter,
                $state,
                new WorkspacePreparer($root.'/workspaces'),
            );

            exclusion_rejects(
                fn () => $supervisor->runOnce(),
                is_string($profileId)
                    ? 'Invalid profile or operation identifier.'
                    : 'Native execution requires a valid profile identity.',
            );

            exclusion_assert($state->load() === null);
            exclusion_assert($store->read('execution') === null);
            exclusion_assert($cleanups === 0);
            exclusion_assert(! $supervisor->runOnce());
        }
    } finally {
        exclusion_remove($root);
    }
};

$tests['pre-entry recovery stops a matching old reservation before acknowledging the claimed attempt'] = function (): void {
    $cleanups = 0;
    [$root, $claim, $store, $sandbox] = exclusion_fixture(function () use (&$cleanups): void {
        $cleanups++;
    });

    try {
        $store->exclusively(fn () => $store->reserveExecution($claim));
        $state = new AttemptStateStore($root.'/active-attempt.json');
        $client = new ExclusionControlPlane(function () use ($store, &$cleanups): void {
            exclusion_assert($cleanups === 1);
            exclusion_assert($store->read('execution') === null);
        }, $claim);
        $supervisor = new Supervisor(
            $client,
            $sandbox,
            new NormalizedExecutionAdapter,
            $state,
            new WorkspacePreparer($root.'/workspaces'),
        );

        exclusion_rejects(fn () => $supervisor->runOnce(), 'Profile execution requires stopped recovery.');

        exclusion_assert($client->acknowledgements === 1);
        exclusion_assert($state->load() === null);
    } finally {
        exclusion_remove($root);
    }
};

$tests['failed stopped acknowledgement retries safely after release and refuses a newer reservation'] = function (): void {
    $cleanups = 0;
    [$root, $claim, $store, $sandbox] = exclusion_fixture(function () use (&$cleanups): void {
        $cleanups++;
    });

    try {
        $rejectAcknowledgement = true;
        $state = new AttemptStateStore($root.'/active-attempt.json');
        $client = new ExclusionControlPlane(function () use (&$rejectAcknowledgement): void {
            if ($rejectAcknowledgement) {
                throw new RuntimeException('Simulated stopped acknowledgement outage.');
            }
        }, $claim);
        $supervisor = new Supervisor(
            $client,
            $sandbox,
            new NormalizedExecutionAdapter,
            $state,
            new WorkspacePreparer($root.'/workspaces'),
        );

        exclusion_rejects(fn () => $supervisor->runOnce(), 'Sandbox cleanup acknowledgement failed; durable state was retained.');
        exclusion_assert($store->read('execution') === null);
        exclusion_assert($state->load() !== null);
        exclusion_assert($client->acknowledgements === 0);

        $newer = exclusion_claim(2);
        $store->exclusively(fn () => $store->reserveExecution($newer));
        $marker = $store->read('execution');
        exclusion_rejects(fn () => $supervisor->reconcileAfterRestart(), 'Profile execution belongs to another attempt.');
        exclusion_assert($store->read('execution') === $marker);
        exclusion_assert($cleanups === 0);
        exclusion_assert($state->load() !== null);

        $sandbox->reconcileProfileExecution($newer, null);
        $rejectAcknowledgement = false;
        $supervisor->reconcileAfterRestart();

        exclusion_assert($cleanups === 2);
        exclusion_assert($client->acknowledgements === 1);
        exclusion_assert($state->load() === null);
        exclusion_assert($store->read('execution') === null);
    } finally {
        exclusion_remove($root);
    }
};

$failures = 0;

foreach ($tests as $name => $test) {
    try {
        $test();
        fwrite(STDOUT, "PASS {$name}\n");
    } catch (Throwable $exception) {
        $failures++;
        fwrite(STDERR, "FAIL {$name}: {$exception->getMessage()}\n");
    }
}

fwrite(STDOUT, count($tests)." exclusion tests, {$failures} failed. Native cleanup and account boundaries are simulated.\n");
exit($failures === 0 ? 0 : 1);
