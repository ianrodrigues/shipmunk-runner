<?php

declare(strict_types=1);

use Shipmunk\Runner\CommandRunner;
use Shipmunk\Runner\Profiles\DockerProfileRuntime;
use Shipmunk\Runner\Profiles\NativeProfile;
use Shipmunk\Runner\Profiles\ProfileStore;

require dirname(__DIR__).'/bootstrap.php';

$root = realpath(sys_get_temp_dir()).'/shipmunk-profile-container-'.bin2hex(random_bytes(8));
mkdir($root, 0700);
$profile = '01kkkkkkkkkkkkkkkkkkkkkkkk';
$store = new ProfileStore($root, $profile);
NativeProfile::initializeHome($store->createHome());
$sibling = new ProfileStore($root, '01nnnnnnnnnnnnnnnnnnnnnnnn');
file_put_contents($sibling->createHome().'/sibling-secret', 'SIMULATED_OTHER_PROFILE_SECRET');
chmod($sibling->home().'/sibling-secret', 0600);
$runtime = new DockerProfileRuntime(getenv('SHIPMUNK_PROFILE_IMAGE') ?: 'shipmunk-profile-test:local');
$sandbox = 'shipmunk-profile-01'.bin2hex(random_bytes(12));
$commands = new CommandRunner;
putenv('OPENAI_API_KEY=conflicting-api-key');
putenv('ANTHROPIC_API_KEY=conflicting-api-key');
putenv('CLAUDE_CODE_USE_BEDROCK=1');
putenv('NODE_OPTIONS=--require=/personal/evil.js');
putenv('CODEX_HOME=/personal/credentials');
$failure = null;
try {
    $runtime->start($sandbox, $store->home());
    foreach (NativeProfile::VERSIONS as $agent => $version) {
        $result = $runtime->run($sandbox, $agent, 'version', static fn () => null);
        if (! NativeProfile::versionMatches($agent, $version, $result)) {
            throw new RuntimeException('Native version did not match the pinned runtime.');
        }
    }
    $configuration = json_decode($commands->mustRun(['docker', 'inspect', $sandbox])->stdout, true)[0];
    $mounts = array_values(array_filter($configuration['Mounts'], static fn (array $mount): bool => $mount['Type'] === 'bind'));
    if (count($mounts) !== 1 || $mounts[0]['Source'] !== $store->home()
        || $configuration['HostConfig']['ReadonlyRootfs'] !== true
        || $configuration['HostConfig']['CapDrop'] !== ['ALL']
        || $configuration['HostConfig']['LogConfig']['Type'] !== 'none') {
        throw new RuntimeException('Credential container isolation mismatch.');
    }
    // Environment is written only by the offline simulation fixture.
    if (! getenv('SHIPMUNK_PROFILE_IMAGE')) {
        $environment = file_get_contents($store->home().'/environment');
        foreach (['conflicting-api-key', 'BEDROCK', 'NODE_OPTIONS', '/personal/'] as $forbidden) {
            if (str_contains($environment, $forbidden)) {
                throw new RuntimeException('Conflicting native provider environment leaked.');
            }
        }
        $probe = $runtime->run($sandbox, 'codex', 'probe', static fn () => null);
        if (NativeProfile::health('codex', $probe)['health'] !== 'ready') {
            throw new RuntimeException('Simulated native status failed.');
        }
        // Prove this fixture would fail if the supervisor accidentally mounted the whole store root.
        $wrongMount = $commands->run([
            'docker', 'run', '--rm', '--network', 'none', '--read-only',
            '--user', posix_geteuid().':'.posix_getegid(),
            '--mount', 'type=bind,src='.$root.',dst=/profile',
            '--entrypoint', '/usr/local/bin/codex', 'shipmunk-profile-test:local', 'login', 'status',
        ]);
        if ($wrongMount->exitCode !== 3) {
            throw new RuntimeException('Sibling-home fixture did not detect an exposed parent store.');
        }

    }
    $runtime->stop($sandbox);
    $store->validateHome();
    fwrite(STDOUT, "PASS real Linux credential-only container: pinned versions, clean environment, sole home mount and protected store. Account auth not executed.\n");
} catch (Throwable $exception) {
    $failure = $exception;
} finally {
    try {
        $runtime->stop($sandbox);
    } catch (Throwable $exception) {
        $failure ??= $exception;
    } finally {
        $store->invalidate();
        $sibling->invalidate();
    }
}
if ($failure !== null) {
    throw $failure;
}
