<?php

declare(strict_types=1);

use Shipmunk\Runner\CommandRunner;
use Shipmunk\Runner\Profiles\NativeProfile;
use Shipmunk\Runner\Profiles\ProfileStore;

require dirname(__DIR__).'/bootstrap.php';

$image = getenv('SHIPMUNK_PROFILE_IMAGE') ?: 'shipmunk-profile-native-test:local';
$root = realpath(sys_get_temp_dir()).'/shipmunk-native-home-'.bin2hex(random_bytes(8));
mkdir($root, 0700);
$profile = '01kkkkkkkkkkkkkkkkkkkkkkkk';
$store = new ProfileStore($root, $profile);
$commands = new CommandRunner;
$sandbox = 'shipmunk-native-home-'.bin2hex(random_bytes(8));

try {
    $store->exclusively(function () use ($store, $commands, $image, $sandbox): void {
        NativeProfile::initializeHome($store->createHome());
        // The real CLI starts in a disposable, unauthenticated home. Network access is
        // disabled; timeout bounds its expected failed request without retaining output.
        $commands->run([
            'docker', 'run', '--rm', '--name', $sandbox, '--network', 'none', '--init', '--read-only',
            '--user', posix_geteuid().':'.posix_getegid(), '--cap-drop', 'ALL',
            '--security-opt', 'no-new-privileges', '--log-driver', 'none',
            '--pids-limit', '64', '--memory', '512m', '--cpus', '1',
            '--tmpfs', '/tmp:rw,nosuid,nodev,noexec,size=64m,mode=1777',
            '--mount', 'type=bind,src='.$store->home().',dst=/profile',
            '--tmpfs', '/profile/.codex/tmp:rw,nosuid,nodev,size=16m,mode=0700,uid='.posix_geteuid().',gid='.posix_getegid(),
            '--entrypoint', '/usr/bin/env', $image,
            '-i', 'HOME=/profile', 'CODEX_HOME=/profile/.codex',
            'XDG_CONFIG_HOME=/profile/.config', 'XDG_CACHE_HOME=/profile/.cache',
            'PATH=/usr/local/bin:/usr/bin:/bin', 'LANG=C.UTF-8', 'TERM=dumb',
            '/bin/sh', '-c', 'umask 077; exec timeout --kill-after=2 5 "$@"', 'native-home-test',
            ...NativeProfile::command('codex', 'preflight'),
        ], timeoutSeconds: 15);

        $inspect = $commands->run(['docker', 'inspect', $sandbox]);
        if ($inspect->exitCode === 0 || preg_match('/no such (?:object|container)/i', $inspect->stdout.$inspect->stderr) !== 1) {
            throw new RuntimeException('Native test container must be absent before normalization.');
        }

        $installation = $store->home().'/.codex/installation_id';
        clearstatcache(true, $installation);
        if (! is_file($installation) || (fileperms($installation) & 0777) !== 0644) {
            throw new RuntimeException('Pinned native CLI did not reproduce the generated metadata permissions.');
        }
        $store->normalizeNativeHome();
        ProfileStore::protect($installation);
        $store->validateHome();

        // Docker Desktop preserves host ownership across bind mounts; Linux CI exercises
        // the foreign-owner boundary on the real filesystem.
        if (PHP_OS_FAMILY === 'Linux') {
            $foreignUid = posix_geteuid() === 65534 ? 65533 : 65534;
            $foreign = $store->home().'/foreign-owned';
            file_put_contents($foreign, 'disposable-fixture');
            chmod($foreign, 0644);
            $commands->mustRun([
                'docker', 'run', '--rm', '--network', 'none', '--read-only', '--user', '0:0',
                '--cap-drop', 'ALL', '--cap-add', 'CHOWN', '--cap-add', 'DAC_OVERRIDE', '--security-opt', 'no-new-privileges',
                '--mount', 'type=bind,src='.$store->home().',dst=/fixture',
                '--entrypoint', '/bin/chown', $image, $foreignUid.':'.$foreignUid, '/fixture/foreign-owned',
            ]);
            try {
                $store->normalizeNativeHome();
                throw new LogicException('Foreign-owned native entries must be rejected.');
            } catch (RuntimeException) {
                clearstatcache(true, $foreign);
                if (fileowner($foreign) !== $foreignUid || (fileperms($foreign) & 0777) !== 0644) {
                    throw new RuntimeException('Rejected foreign-owned entry was modified.');
                }
            }
        } else {
            fwrite(STDOUT, "SKIP foreign-owner fixture: requires Linux bind-mount ownership semantics.\n");
        }
    });
} finally {
    $commands->run(['docker', 'rm', '--force', $sandbox]);
    $store->invalidate();
    @unlink($root.'/'.$profile.'/lock');
    @rmdir($root.'/'.$profile);
    @rmdir($root);
}

fwrite(STDOUT, "PASS real pinned Codex metadata normalized after stop. No account or network access.\n");
