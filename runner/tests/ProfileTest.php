<?php

declare(strict_types=1);

use Shipmunk\Runner\CommandResult;
use Shipmunk\Runner\ControlPlaneException;
use Shipmunk\Runner\HttpResponse;
use Shipmunk\Runner\HttpTransport;
use Shipmunk\Runner\Profiles\HttpProfileControlPlane;
use Shipmunk\Runner\Profiles\NativeProfile;
use Shipmunk\Runner\Profiles\ProfileControlPlane;
use Shipmunk\Runner\Profiles\ProfileExecutionGate;
use Shipmunk\Runner\Profiles\ProfileLifecycle;
use Shipmunk\Runner\Profiles\ProfileRuntime;
use Shipmunk\Runner\Profiles\ProfileStore;
use Shipmunk\Runner\Watchdog;
use Shipmunk\Runner\Setup\SetupWizard;
use Shipmunk\Runner\WatchdogLease;

require dirname(__DIR__).'/bootstrap.php';

// All account responses are simulations. No native executable or live account is used here.
const PROFILE = '01kkkkkkkkkkkkkkkkkkkkkkkk';
const OPERATION = '01mmmmmmmmmmmmmmmmmmmmmmmm';
const NEXT_OPERATION = '01nnnnnnnnnnnnnnnnnnnnnnnn';

function profile_assert(bool $value): void
{
    if (! $value) {
        throw new RuntimeException('Profile assertion failed.');
    }
}

function profile_throws(Closure $callback): void
{
    try {
        $callback();
    } catch (RuntimeException) {
        return;
    }
    throw new RuntimeException('Expected profile operation rejection.');
}

function profile_fixture(): array
{
    $root = realpath(sys_get_temp_dir()).'/shipmunk-profile-test-'.bin2hex(random_bytes(8));
    mkdir($root, 0700);

    return [$root, new ProfileStore($root, PROFILE)];
}

final class SimulatedProfileControlPlane implements ProfileControlPlane
{
    public array $requests = [];

    public bool $active = true;

    public bool $stop = false;

    public bool $stopped = false;

    public bool $reorder = false;

    public bool $omitVersion = false;

    public bool $failCompletion = false;

    public bool $failHeartbeat = false;

    public ?int $completionStatus = null;

    public string $agent = 'codex';

    public string $mode = 'subscription';

    public string $version = '0.154.0';

    public string $operationId = OPERATION;

    public function request(string $profileId, string $suffix, array $payload): array
    {
        $this->requests[] = [$suffix, $payload];
        if ($suffix === 'operations') {
            $this->operationId = $payload['operation_id'];
        }
        if (str_ends_with($suffix, '/heartbeat') && $this->failHeartbeat) {
            throw new RuntimeException('SECRET_TOKEN https://private-provider.test /private/profile-path');
        }
        if (str_ends_with($suffix, '/completion') && $this->completionStatus !== null) {
            throw new ControlPlaneException($this->completionStatus, 'Simulated completion response.');
        }
        if (str_ends_with($suffix, '/completion') && $this->failCompletion) {
            throw new RuntimeException('Simulated lost completion response with secret bytes.');
        }

        $response = [
            'profile_id' => $profileId,
            'operation_id' => $this->operationId,
            'agent' => $this->agent,
            'auth_mode' => $this->mode,
            'runtime_version' => $this->version,
            'credential_reference' => 'credential:fixture',
            'lease_expires_at' => (new DateTimeImmutable('+45 seconds'))->format(DATE_ATOM),
            'stop_requested' => $this->stop,
            'active' => $this->active,
            'stopped' => $this->stopped,
            'health' => $this->active ? 'ready' : 'disconnected',
        ];
        if ($this->omitVersion) {
            unset($response['runtime_version']);
        }

        return $this->reorder && count($this->requests) % 2 === 0 ? array_reverse($response, true) : $response;
    }
}

final class SimulatedProfileRuntime implements ProfileRuntime
{
    public array $commands = [];

    public int $starts = 0;

    public bool $stopped = true;

    public bool $failStop = false;

    public ?Closure $onProbe = null;

    public ?string $failCommand = null;

    public ?Closure $onStop = null;

    public ?CommandResult $probe = null;

    public ?string $version = null;

    public ?CommandResult $preflight = null;

    private string $home;

    public function start(string $sandbox, string $home): void
    {
        $this->starts++;
        $this->home = $home;
        $this->stopped = false;
    }

    public function run(string $sandbox, string $agent, string $command, Closure $checkpoint): CommandResult
    {
        $checkpoint();
        $this->commands[] = $command;
        if ($this->failCommand === $command) {
            throw new RuntimeException('SECRET_TOKEN https://private-provider.test /private/profile-path');
        }
        if ($command === 'version') {
            return new CommandResult(0, $this->version ?? ($agent === 'codex' ? 'codex-cli 0.154.0' : '2.1.269 (Claude Code)'), '');
        }
        if ($command === 'login') {
            file_put_contents($this->home.'/simulated-secret', 'FAKE_NATIVE_SECRET');
            chmod($this->home.'/simulated-secret', 0600);

            return new CommandResult(0, '', '');
        }
        if ($command === 'preflight') {
            return $this->preflight ?? new CommandResult(0, "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"SHIPMUNK_AUTH_OK\"}}\n{\"type\":\"turn.completed\"}", '');
        }
        ($this->onProbe ?? static fn () => null)();

        return $this->probe ?? new CommandResult(0, '', 'Logged in using ChatGPT');
    }

    public function stop(string $sandbox): void
    {
        ($this->onStop ?? static fn () => null)();
        if ($this->failStop) {
            throw new RuntimeException('Simulated process cleanup failure.');
        }
        $this->stopped = true;
    }
}

final class SimulatedProfileWatchdog implements Watchdog
{
    public function arm(string $sandboxId, DateTimeImmutable $lease, DateTimeImmutable $deadline): WatchdogLease
    {
        return new class implements WatchdogLease
        {
            public function renew(DateTimeImmutable $lease): void {}

            public function disarm(): void {}
        };
    }
}

function profile_lifecycle(SimulatedProfileControlPlane $api, SimulatedProfileRuntime $runtime): ProfileLifecycle
{
    return new ProfileLifecycle($api, $runtime, new SimulatedProfileWatchdog);
}

$tests = [];
$tests['simulated login activates only after subscription probe and server acceptance'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $runtime = new SimulatedProfileRuntime;
    $runtime->onProbe = static function () use ($store): void {
        profile_assert($store->read('active') === null);
    };
    $result = profile_lifecycle($api, $runtime)->operate($store, PROFILE, 'login', OPERATION);
    profile_assert($result === ['health' => 'ready', 'reason' => null]);
    profile_assert($runtime->commands === ['version', 'login', 'probe', 'preflight', 'probe']);
    profile_assert($runtime->stopped && $store->read('active')['credential_reference'] === 'credential:fixture');
    profile_assert(! str_contains(json_encode($api->requests), 'FAKE_NATIVE_SECRET'));
    profile_throws(fn () => profile_lifecycle($api, $runtime)->operate($store, PROFILE, 'login', OPERATION));
    $store->invalidate();
};
$tests['simulated lifecycle protects generated metadata only after native processes stop'] = function (): void {
    foreach ([false, true] as $failStop) {
        [$root, $store] = profile_fixture();
        $api = new SimulatedProfileControlPlane;
        $runtime = new SimulatedProfileRuntime;
        $runtime->failStop = $failStop;
        $file = $store->home().'/native-metadata';
        $runtime->onProbe = static function () use ($file): void {
            file_put_contents($file, 'simulated-native-metadata');
            chmod($file, 0644);
        };
        $runtime->onStop = static function () use ($file, $runtime): void {
            if (! $runtime->stopped) {
                clearstatcache(true, $file);
                profile_assert((fileperms($file) & 0777) === 0644);
            }
        };
        $operate = fn () => profile_lifecycle($api, $runtime)->operate($store, PROFILE, 'login', OPERATION);
        if ($failStop) {
            profile_throws($operate);
            clearstatcache(true, $file);
            profile_assert((fileperms($file) & 0777) === 0644);
            profile_assert($store->read('active') === null);
        } else {
            profile_assert($operate()['health'] === 'ready');
            ProfileStore::protect($file);
            profile_assert($runtime->stopped);
        }
        $store->invalidate();
    }
};
$tests['simulated Claude requires explicit subscription status and rejects Console or unknown billing'] = function (): void {
    foreach (['pro', 'max', 'team', 'enterprise', null, 'api'] as $subscription) {
        $status = new CommandResult(0, json_encode([
            'loggedIn' => true,
            'authMethod' => 'claude.ai',
            'apiProvider' => 'firstParty',
            'subscriptionType' => $subscription,
        ]), '');
        profile_assert(NativeProfile::health('claude_code', $status)['health'] === (in_array($subscription, ['pro', 'max', 'team', 'enterprise'], true) ? 'ready' : 'unsupported'));
    }
    profile_assert(NativeProfile::health('codex', new CommandResult(0, 'Logged in using an API key', ''))['health'] === 'unsupported');
    profile_assert(NativeProfile::health('codex', new CommandResult(1, 'Logged in using ChatGPT', ''))['health'] === 'expired');
};
$tests['simulated unknown mode and runtime mismatch never launch native login'] = function (): void {
    foreach (['api_key', 'unknown', 'version'] as $case) {
        [$root, $store] = profile_fixture();
        $api = new SimulatedProfileControlPlane;
        $runtime = new SimulatedProfileRuntime;
        $case === 'version' ? $api->version = '99.0.0' : $api->mode = $case;
        $result = profile_lifecycle($api, $runtime)->operate($store, PROFILE, 'login', OPERATION);
        profile_assert($result['health'] === 'unsupported' && $runtime->starts === 0 && $store->read('active') === null);
    }
};
$tests['simulated executable version mismatch prevents login and invalidates credentials'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $runtime = new SimulatedProfileRuntime;
    $runtime->version = 'codex-cli 0.155.0';
    $result = profile_lifecycle($api, $runtime)->operate($store, PROFILE, 'login', OPERATION);
    profile_assert($result['reason'] === 'runtime_mismatch' && $runtime->commands === ['version'] && ! is_dir($store->home()));
};
$tests['simulated failed refresh invalidates existing home without API fallback'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $runtime = new SimulatedProfileRuntime;
    $lifecycle = profile_lifecycle($api, $runtime);
    $lifecycle->operate($store, PROFILE, 'login', OPERATION);
    $runtime->probe = new CommandResult(1, 'PRIVATE_PROVIDER_FAILURE', '');
    $result = $lifecycle->operate($store, PROFILE, 'probe', NEXT_OPERATION);
    profile_assert($result['health'] === 'expired' && ! is_dir($store->home()));
    profile_assert(! str_contains(json_encode($api->requests), 'PRIVATE_PROVIDER_FAILURE'));
};
$tests['simulated revocation during probe removes credentials and acknowledges only stopped native tree'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $runtime = new SimulatedProfileRuntime;
    $runtime->onProbe = static function () use ($api): void {
        $api->stop = true;
    };
    $result = profile_lifecycle($api, $runtime)->operate($store, PROFILE, 'login', OPERATION);
    profile_assert($result['health'] === 'error' && $runtime->stopped && $store->read('active') === null && ! is_dir($store->home()));
    profile_assert(end($api->requests)[1]['stopped'] === true);
};
$tests['simulated revocation at completion cannot activate the staged home'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $api->active = false;
    $runtime = new SimulatedProfileRuntime;
    $result = profile_lifecycle($api, $runtime)->operate($store, PROFILE, 'login', OPERATION);
    profile_assert($result['health'] === 'disconnected' && $store->read('active') === null && ! is_dir($store->home()));
};
$tests['simulated lost completion is recovered by cleanup without replaying login'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $api->failCompletion = true;
    $runtime = new SimulatedProfileRuntime;
    $lifecycle = profile_lifecycle($api, $runtime);
    profile_throws(fn () => $lifecycle->operate($store, PROFILE, 'login', OPERATION));
    profile_assert($store->read('pending') !== null && $store->read('active') === null && $runtime->stopped);
    $api->failCompletion = false;
    profile_throws(fn () => $lifecycle->operate($store, PROFILE, 'login', OPERATION));
    profile_assert($runtime->starts === 1 && $store->read('pending') === null && $store->read('active') !== null && is_dir($store->home()));
};
$tests['guided connection IDs recover interrupted completion then start a fresh operation'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $api->failCompletion = true;
    $runtime = new SimulatedProfileRuntime;
    $lifecycle = profile_lifecycle($api, $runtime);
    $original = SetupWizard::operationId();
    profile_throws(fn () => $lifecycle->operate($store, PROFILE, 'login', $original));
    profile_assert($store->read('pending')['operation_id'] === $original);

    $api->failCompletion = false;
    $next = SetupWizard::operationId();
    $result = $lifecycle->operate($store, PROFILE, 'probe', $next);

    profile_assert($result['health'] === 'ready');
    profile_assert($store->read('completed')['operation_id'] === $next);
    profile_assert($store->read('pending') === null && $store->read('active') !== null);
    profile_assert($runtime->starts === 2);
    $completions = array_filter($api->requests, fn (array $request): bool => $request[0] === 'operations/'.$original.'/completion');
    profile_assert(count($completions) === 2);
};
$tests['simulated cleanup failure retains exclusion and prevents stopped acknowledgement'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $runtime = new SimulatedProfileRuntime;
    $runtime->failStop = true;
    profile_throws(fn () => profile_lifecycle($api, $runtime)->operate($store, PROFILE, 'login', OPERATION));
    profile_assert($store->read('pending') !== null && $store->read('active') === null);
    profile_assert(! str_contains(json_encode($api->requests), '/completion'));
};
$tests['real OS flock excludes independent profile login refresh disconnect and execution holders'] = function (): void {
    [$root, $store] = profile_fixture();
    $store->exclusively(static function () use ($root): void {
        $pid = pcntl_fork();
        if ($pid === 0) {
            try {
                (new ProfileStore($root, PROFILE))->exclusively(fn () => null);
                exit(1);
            } catch (RuntimeException) {
                exit(0);
            }
        }
        pcntl_waitpid($pid, $status);
        profile_assert(pcntl_wexitstatus($status) === 0);
        (new ProfileStore($root, NEXT_OPERATION))->exclusively(fn () => null);
    });
    $store->exclusively(fn () => null);
};
$tests['profile store rejects traversal symlinks hardlinks and broad permissions without touching other homes'] = function (): void {
    [$root, $store] = profile_fixture();
    profile_throws(fn () => new ProfileStore($root, '../escape'));
    mkdir($root.'/target', 0700);
    symlink($root.'/target', $root.'/'.NEXT_OPERATION);
    profile_throws(fn () => new ProfileStore($root, NEXT_OPERATION));
    $store->createHome();
    file_put_contents($root.'/target/secret', 'untouched');
    symlink($root.'/target', $store->home().'/link');
    profile_throws(fn () => $store->validateHome());
    $store->invalidate();
    profile_assert(file_get_contents($root.'/target/secret') === 'untouched');
    $store->createHome();
    link($root.'/target/secret', $store->home().'/hardlink');
    profile_throws(fn () => $store->validateHome());
    $store->invalidate();
    chmod($root, 0755);
    profile_throws(fn () => new ProfileStore($root, PROFILE));
    chmod($root, 0700);
};
$tests['native home normalization requires its lock and protects only safe owned entries'] = function (): void {
    [$root, $store] = profile_fixture();
    mkdir($store->createHome().'/native', 0755);
    $file = $store->home().'/native/installation_id';
    file_put_contents($file, 'simulated-native-metadata');
    chmod($file, 0644);
    profile_throws(fn () => $store->validateHome());
    profile_throws(fn () => $store->normalizeNativeHome());
    [$clone, $serialized] = $store->exclusively(fn () => [clone $store, serialize($store)]);
    profile_throws(fn () => $clone->normalizeNativeHome());
    profile_throws(fn () => unserialize($serialized)->normalizeNativeHome());
    $store->exclusively(fn () => $store->normalizeNativeHome());
    ProfileStore::protect($file);
    ProfileStore::protect(dirname($file), true);
    profile_assert(file_get_contents($file) === 'simulated-native-metadata');
    $store->invalidate();
};
$tests['native home normalization rejects links and special entries before changing any file'] = function (): void {
    foreach (['symlink', 'directory_link', 'hardlink', 'fifo', 'special_mode'] as $unsafe) {
        [$root, $store] = profile_fixture();
        $home = $store->createHome();
        file_put_contents($home.'/ordinary', 'native-metadata');
        chmod($home.'/ordinary', 0644);
        file_put_contents($root.'/outside', 'outside');
        chmod($root.'/outside', 0644);
        $path = $home.'/unsafe';
        match ($unsafe) {
            'symlink' => symlink($root.'/outside', $path),
            'directory_link' => symlink($root, $path),
            'hardlink' => link($root.'/outside', $path),
            'fifo' => posix_mkfifo($path, 0600),
            'special_mode' => file_put_contents($path, 'native-metadata'),
        };
        if ($unsafe === 'special_mode') {
            chmod($path, 04600);
        }
        $store->exclusively(fn () => profile_throws(fn () => $store->normalizeNativeHome()));
        clearstatcache();
        profile_assert((fileperms($home.'/ordinary') & 0777) === 0644);
        profile_assert((fileperms($root.'/outside') & 0777) === 0644);
        unlink($path);
        $store->invalidate();
    }
};
$tests['native home normalization keeps enclosing directories strictly protected'] = function (): void {
    foreach (['root', 'profile', 'home'] as $ancestor) {
        [$root, $store] = profile_fixture();
        $home = $store->createHome();
        $path = match ($ancestor) {
            'root' => $root,
            'profile' => dirname($home),
            'home' => $home,
        };
        chmod($path, 0755);
        $store->exclusively(fn () => profile_throws(fn () => $store->normalizeNativeHome()));
        clearstatcache();
        profile_assert((fileperms($path) & 0777) === 0755);
        chmod($path, 0700);
        $store->invalidate();
    }
};
$tests['simulated disconnect deletes the local home without promising provider token revocation'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $runtime = new SimulatedProfileRuntime;
    $lifecycle = profile_lifecycle($api, $runtime);
    $lifecycle->operate($store, PROFILE, 'login', OPERATION);
    $result = $lifecycle->operate($store, PROFILE, 'disconnect', NEXT_OPERATION);
    profile_assert($result['health'] === 'disconnected' && $runtime->starts === 1 && ! is_dir($store->home()));
};

$tests['simulated historical begin replay does not destroy a newer active home'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $runtime = new SimulatedProfileRuntime;
    $lifecycle = profile_lifecycle($api, $runtime);
    $lifecycle->operate($store, PROFILE, 'login', OPERATION);
    $lifecycle->operate($store, PROFILE, 'login', NEXT_OPERATION);
    $identity = $store->read('active');
    $api->stop = true;
    $api->stopped = true;
    profile_throws(fn () => $lifecycle->operate($store, PROFILE, 'login', OPERATION));
    profile_assert($store->read('active') === $identity && is_file($store->home().'/simulated-secret'));
    profile_assert($store->read('pending') === null && $runtime->starts === 2);
};

$tests['simulated cached login cannot become ready when authenticated preflight fails'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $runtime = new SimulatedProfileRuntime;
    $runtime->preflight = new CommandResult(0, '{"type":"turn.failed","error":{"message":"PRIVATE_REVOKED_TOKEN"}}', '');
    $result = profile_lifecycle($api, $runtime)->operate($store, PROFILE, 'login', OPERATION);
    profile_assert($result === ['health' => 'error', 'reason' => 'native_probe_failed']);
    profile_assert($store->read('active') === null && ! is_dir($store->home()));
    profile_assert(! str_contains(json_encode($api->requests), 'PRIVATE_REVOKED_TOKEN'));
};
$tests['simulated expired unfinished begin is acknowledged stopped rather than forgetting its lease'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $api->stop = true;
    $runtime = new SimulatedProfileRuntime;
    profile_throws(fn () => profile_lifecycle($api, $runtime)->operate($store, PROFILE, 'login', OPERATION));
    profile_assert(end($api->requests)[0] === 'operations/'.OPERATION.'/completion');
    profile_assert(end($api->requests)[1]['stopped'] === true && $runtime->starts === 0 && $store->read('pending') === null);
};
$tests['simulated malformed and tool-bearing authenticated results fail closed'] = function (): void {
    foreach (['', '{}', '{"type":"turn.completed"}', "{\"type\":\"item.completed\",\"item\":{\"type\":\"command_execution\"}}\n{\"type\":\"turn.completed\"}"] as $output) {
        profile_assert(NativeProfile::authenticatedHealth('codex', new CommandResult(0, $output, ''))['health'] === 'error');
    }
    $success = '{"type":"result","subtype":"success","is_error":false,"result":"SHIPMUNK_AUTH_OK"}';
    profile_assert(NativeProfile::authenticatedHealth('claude_code', new CommandResult(0, $success, ''))['health'] === 'ready');
    profile_assert(NativeProfile::authenticatedHealth('claude_code', new CommandResult(1, $success, ''))['health'] === 'error');
};

$tests['simulated response key order does not invalidate ready credentials or reject execution identity'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $api->reorder = true;
    $runtime = new SimulatedProfileRuntime;
    $lifecycle = profile_lifecycle($api, $runtime);
    profile_assert($lifecycle->operate($store, PROFILE, 'login', OPERATION)['health'] === 'ready');
    profile_assert($lifecycle->operate($store, PROFILE, 'probe', NEXT_OPERATION)['health'] === 'ready');
    $binding = array_reverse($store->read('active'), true);
    $binding['extra_transport_field'] = true;
    $authorized = false;
    $value = (new ProfileExecutionGate)->execute($store, $binding, static function () use (&$authorized): void {
        $authorized = true;
    }, static fn (string $home): string => $home);
    profile_assert($authorized && $value === $store->home());
};
$tests['malformed native result values return sanitized probe failure'] = function (): void {
    foreach ([null, [], new stdClass, 42, true] as $value) {
        $output = json_encode(['type' => 'result', 'subtype' => 'success', 'is_error' => false, 'result' => $value]);
        profile_assert(NativeProfile::authenticatedHealth('claude_code', new CommandResult(0, $output, ''))['reason'] === 'native_probe_failed');
    }
};
$tests['scalar control-plane JSON is rejected through the expected protocol error'] = function (): void {
    foreach (['"ok"', 'null', '42', 'true'] as $body) {
        $transport = new class($body) implements HttpTransport
        {
            public function __construct(private string $body) {}

            public function request(string $method, string $url, array $headers, string $body, int $timeoutSeconds, int $maxResponseBytes): HttpResponse
            {
                return new HttpResponse(200, [], $this->body);
            }
        };
        $client = new HttpProfileControlPlane('http://localhost', 'simulated-token', $transport);
        profile_throws(fn () => $client->request(PROFILE, 'operations', []));
    }
};
$tests['missing runtime version retains recovery state without native execution or malformed completion'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $api->omitVersion = true;
    $runtime = new SimulatedProfileRuntime;
    profile_throws(fn () => profile_lifecycle($api, $runtime)->operate($store, PROFILE, 'login', OPERATION));
    profile_assert($runtime->starts === 0 && $store->read('pending') !== null && count($api->requests) === 1);
};
$tests['failed journal activation removes temporary residue'] = function (): void {
    [$root, $store] = profile_fixture();
    mkdir($root.'/'.PROFILE.'/active.json', 0700);
    set_error_handler(static function (int $severity, string $message): never {
        throw new RuntimeException($message);
    });
    try {
        profile_throws(fn () => $store->write('active', ['credential_reference' => 'credential:simulated']));
    } finally {
        restore_error_handler();
    }
    profile_assert(glob($root.'/'.PROFILE.'/active.json.*') === []);
};

$tests['unverifiable ready recovery invalidates credentials but retains duplicate ready acknowledgements'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $api->failCompletion = true;
    $runtime = new SimulatedProfileRuntime;
    $lifecycle = profile_lifecycle($api, $runtime);
    profile_throws(fn () => $lifecycle->operate($store, PROFILE, 'login', OPERATION));
    chmod($store->home().'/simulated-secret', 0644);
    $api->failCompletion = false;
    $api->stopped = true;
    profile_throws(fn () => $lifecycle->operate($store, PROFILE, 'login', OPERATION));
    profile_assert(! is_dir($store->home()) && $store->read('active') === null);
    profile_assert($store->read('pending')['recovery_failed'] === true && $runtime->starts === 1);
    profile_assert(end($api->requests)[1]['reason'] === 'operation_stopped');
    // Product disconnect invalidates the earlier ready operation and confirms local recovery.
    $api->active = false;
    profile_throws(fn () => $lifecycle->operate($store, PROFILE, 'login', OPERATION));
    profile_assert($store->read('pending') === null && $runtime->starts === 1);
};

$tests['failed home recovery releases only a missing operation and retains ambiguous HTTP failures'] = function (): void {
    foreach ([404, 409, 422] as $status) {
        [$root, $store] = profile_fixture();
        $api = new SimulatedProfileControlPlane;
        $api->failCompletion = true;
        $runtime = new SimulatedProfileRuntime;
        $lifecycle = profile_lifecycle($api, $runtime);
        profile_throws(fn () => $lifecycle->operate($store, PROFILE, 'login', OPERATION));
        $link = $store->home().'/unsafe-link';
        if (! is_link($link)) {
            symlink('/outside-profile-secret-path', $link);
        }
        $api->failCompletion = false;
        $api->completionStatus = $status;
        profile_throws(fn () => $lifecycle->operate($store, PROFILE, 'login', OPERATION));
        profile_assert(($store->read('pending') === null) === ($status === 404));
        profile_assert(! is_dir($store->home()) && $store->read('active') === null);
    }
};

$tests['missing completed operations terminate ordinary recovery without activating cached credentials'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $api->failCompletion = true;
    $runtime = new SimulatedProfileRuntime;
    $lifecycle = profile_lifecycle($api, $runtime);
    profile_throws(fn () => $lifecycle->operate($store, PROFILE, 'login', OPERATION));
    $api->failCompletion = false;
    $api->completionStatus = 404;
    profile_throws(fn () => $lifecycle->operate($store, PROFILE, 'login', OPERATION));
    profile_assert($store->read('pending') === null && $store->read('active') === null && ! is_dir($store->home()));
    profile_assert($runtime->starts === 1);
};

$tests['simulated diagnostics expose only the failed native stage and preserve API reasons'] = function (): void {
    foreach (['version', 'login', 'probe', 'preflight'] as $command) {
        [$root, $store] = profile_fixture();
        $api = new SimulatedProfileControlPlane;
        $runtime = new SimulatedProfileRuntime;
        $runtime->failCommand = $command;
        $stages = [];
        $lifecycle = new ProfileLifecycle($api, $runtime, new SimulatedProfileWatchdog, static function (string $stage) use (&$stages): void {
            $stages[] = $stage;
        });

        $result = $lifecycle->operate($store, PROFILE, 'login', OPERATION);

        profile_assert($stages === ['native_'.$command]);
        profile_assert($result === ['health' => 'error', 'reason' => 'operation_failed']);
        profile_assert($runtime->stopped && $store->read('pending') === null && $store->read('active') === null);
        profile_assert(! str_contains(json_encode([$stages, $api->requests]), 'SECRET_TOKEN'));
        profile_assert(! str_contains(json_encode([$stages, $api->requests]), 'private-provider.test'));
        profile_assert(! str_contains(json_encode([$stages, $api->requests]), 'private/profile-path'));
    }
};
$tests['simulated post-login home validation reports its stage without filesystem details'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $runtime = new SimulatedProfileRuntime;
    $runtime->onProbe = static function () use ($store): void {
        $link = $store->home().'/unsafe-link';
        if (! is_link($link)) {
            symlink('/outside-profile-secret-path', $link);
        }
    };
    $stages = [];
    $lifecycle = new ProfileLifecycle($api, $runtime, new SimulatedProfileWatchdog, static function (string $stage) use (&$stages): void {
        $stages[] = $stage;
    });

    $result = $lifecycle->operate($store, PROFILE, 'login', OPERATION);

    profile_assert($stages === ['post_auth_home_validation']);
    profile_assert($result['reason'] === 'operation_failed' && $runtime->stopped);
    profile_assert(! str_contains(json_encode([$stages, $api->requests]), $root));
};
$tests['simulated heartbeat failures report their own stage'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $api->failHeartbeat = true;
    $runtime = new SimulatedProfileRuntime;
    $stages = [];
    $lifecycle = new ProfileLifecycle($api, $runtime, new SimulatedProfileWatchdog, static function (string $stage) use (&$stages): void {
        $stages[] = $stage;
    });

    $result = $lifecycle->operate($store, PROFILE, 'login', OPERATION);

    profile_assert($stages === ['heartbeat']);
    profile_assert($result['reason'] === 'operation_failed' && $runtime->stopped);
    profile_assert($store->read('pending') === null);
};
$tests['simulated failing diagnostic callbacks do not interrupt cleanup or acknowledgement'] = function (): void {
    [$root, $store] = profile_fixture();
    $api = new SimulatedProfileControlPlane;
    $runtime = new SimulatedProfileRuntime;
    $runtime->failCommand = 'probe';
    $lifecycle = new ProfileLifecycle($api, $runtime, new SimulatedProfileWatchdog, static function (string $stage): void {
        throw new RuntimeException('DIAGNOSTIC_SECRET');
    });

    $result = $lifecycle->operate($store, PROFILE, 'login', OPERATION);

    profile_assert($result['reason'] === 'operation_failed' && $runtime->stopped);
    profile_assert($store->read('pending') === null && $store->read('active') === null);
    profile_assert(! str_contains(json_encode($api->requests), 'DIAGNOSTIC_SECRET'));
};

$failed = 0;
foreach ($tests as $name => $test) {
    try {
        $test();
        fwrite(STDOUT, "PASS {$name}\n");
    } catch (Throwable $exception) {
        $failed++;
        fwrite(STDERR, "FAIL {$name}: {$exception->getMessage()}\n");
    }
}
fwrite(STDOUT, count($tests).' profile tests, '.$failed." failed. Native account boundaries are simulated.\n");
exit($failed === 0 ? 0 : 1);
