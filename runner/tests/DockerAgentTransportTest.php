<?php

declare(strict_types=1);

use Shipmunk\Runner\CommandRunner;
use Shipmunk\Runner\Drivers\DockerAgentTransport;

require dirname(__DIR__).'/bootstrap.php';

// These tests run real Docker isolation with synthetic native programs, never accounts or models.
$image = getenv('SHIPMUNK_CODEX_TEST_IMAGE') ?: 'shipmunk-profile-native-test:local';
$commands = new CommandRunner;

function transport_assert(
    bool $value,
    string $message,
): void {
    if (! $value) {
        throw new RuntimeException($message);
    }
}

function transport_fixture(
    string $image,
    ?Closure $checkpoint = null,
    int $maxCommands = 100,
): array {
    $root = realpath(sys_get_temp_dir()).'/shipmunk-codex-transport-'.bin2hex(random_bytes(8));
    mkdir($root, 0700);
    foreach (['home', 'home/.codex', 'workspace', 'workspace/sources', 'workspace/sources/0'] as $path) {
        mkdir($root.'/'.$path, 0700);
    }
    file_put_contents($root.'/home/synthetic-secret', 'SYNTHETIC_ACCOUNT_SECRET');
    chmod($root.'/home/synthetic-secret', 0600);
    file_put_contents($root.'/workspace/sources/0/fixture.txt', "original\n");
    $name = 'shipmunk-codex-01'.bin2hex(random_bytes(12)).'-1';

    return [
        $root,
        $name,
        new DockerAgentTransport(
            $name,
            $root.'/home',
            $root.'/workspace',
            $image,
            $image,
            $checkpoint ?? static function (int $requiredSeconds): void {},
            $maxCommands,
        ),
    ];
}

function transport_remove(string $root): void
{
    foreach (new RecursiveIteratorIterator(
        new RecursiveDirectoryIterator($root, FilesystemIterator::SKIP_DOTS),
        RecursiveIteratorIterator::CHILD_FIRST,
    ) as $entry) {
        $entry->isDir() && ! $entry->isLink() ? rmdir($entry->getPathname()) : unlink($entry->getPathname());
    }
    rmdir($root);
}

function transport_absent(string $name): void
{
    $commands = new CommandRunner;
    foreach ([$name, $name.'-repo', $name.'-diff'] as $container) {
        $inspection = $commands->run(['docker', 'inspect', $container]);
        transport_assert(
            $inspection->exitCode !== 0 && str_contains(strtolower($inspection->stderr), 'no such'),
            'Agent sibling survived transport cleanup.',
        );
    }
    $volume = $commands->run(['docker', 'volume', 'inspect', $name.'-workspace']);
    transport_assert($volume->exitCode !== 0 && str_contains(strtolower($volume->stderr), 'no such volume'), 'Workspace memory volume survived cleanup.');
}

function transport_rejects(
    string $name,
    Closure $action,
    string $expectedMessage,
): void {
    try {
        $action();
    } catch (RuntimeException $exception) {
        transport_assert($exception->getMessage() === $expectedMessage, $name.': unexpected failure: '.$exception->getMessage());
        transport_assert(! str_contains($exception->getMessage(), 'SYNTHETIC_ACCOUNT_SECRET'), 'Native credentials leaked in exception.');
        fwrite(STDOUT, "PASS {$name}\n");

        return;
    }
    throw new RuntimeException($name.': failure was not rejected.');
}

$client = <<<'JS'
const fs = require('fs');
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
async function command(id, command) {
    fs.writeFileSync('/bridge/request.tmp', JSON.stringify({ id, command }), { mode: 0o600 });
    fs.renameSync('/bridge/request.tmp', '/bridge/request.json');
    for (let attempt = 0; attempt < 2000; attempt++) {
        try {
            const result = JSON.parse(fs.readFileSync('/bridge/response.json', 'utf8'));
            if (result.id === id) return result;
        } catch {}
        await delay(5);
    }
    throw new Error('Synthetic bridge timed out');
}
JS;

[$root, $name, $transport] = transport_fixture($image);
try {
    transport_absent($name);
    $transport->start();
    $native = json_decode($commands->mustRun(['docker', 'inspect', $name])->stdout, true, flags: JSON_THROW_ON_ERROR)[0];
    $repo = json_decode($commands->mustRun(['docker', 'inspect', $name.'-repo'])->stdout, true, flags: JSON_THROW_ON_ERROR)[0];
    transport_assert($native['HostConfig']['NetworkMode'] === 'bridge', 'Native provider network is unavailable.');
    transport_assert($repo['HostConfig']['NetworkMode'] === 'none', 'Repository acquired network access.');
    foreach ([$native, $repo] as $inspection) {
        transport_assert($inspection['HostConfig']['ReadonlyRootfs'] === true, 'Writable sandbox root.');
        transport_assert($inspection['Config']['User'] === posix_geteuid().':'.posix_getegid(), 'Unexpected sandbox identity.');
        transport_assert($inspection['HostConfig']['CapDrop'] === ['ALL'], 'Container retained capabilities.');
        transport_assert(in_array('no-new-privileges:true', $inspection['HostConfig']['SecurityOpt'], true), 'Container can gain privileges.');
        transport_assert($inspection['HostConfig']['LogConfig']['Type'] === 'none', 'Native text can enter Docker logs.');
    }
    $nativeBindings = array_values(array_filter($native['Mounts'], static fn (array $mount): bool => $mount['Type'] === 'bind'));
    $destinations = array_column($nativeBindings, 'Destination');
    sort($destinations);
    transport_assert($destinations === ['/bridge', '/profile'], 'Native container received repository sources.');
    transport_assert(array_filter($repo['Mounts'], static fn (array $mount): bool => $mount['Type'] === 'bind') === [], 'Repository has a host bind mount.');
    putenv('OPENAI_API_KEY=SYNTHETIC_ACCOUNT_SECRET');
    $result = $transport->run(['node', '-e', $client.<<<'JS'
(async () => {
    if (process.env.OPENAI_API_KEY) throw new Error('API environment was inherited');
    if (fs.existsSync('/workspace/fixture.txt')) throw new Error('Native received source files');
    const result = await command(1, 'test ! -e /profile/synthetic-secret && test ! -e /bridge/request.json && test -z "$OPENAI_API_KEY" && cat fixture.txt && printf "changed\\n" > fixture.txt && printf "new\\n" > added.txt && git add --all && git -c user.name=attacker -c user.email=attacker@example.test commit -qm hidden');
    console.log(JSON.stringify(result));
})();
JS], '', static function (int $requiredSeconds): void {});
    putenv('OPENAI_API_KEY');
    transport_assert($result->exitCode === 0, 'Synthetic native process failed.');
    $response = json_decode($result->stdout, true, flags: JSON_THROW_ON_ERROR);
    transport_assert($response['stdout'] === "original\n" && $response['exit_code'] === 0, 'Repository command did not execute in isolated workspace.');
    $patch = $transport->patch();
    transport_assert(is_string($patch) && str_contains($patch, '+changed') && str_contains($patch, '+new'), 'Trusted diff omitted modified or new files.');
    transport_assert($transport->changedFiles() === [
        [
            'path' => 'added.txt',
            'before_sha256' => null,
            'after_sha256' => hash('sha256', "new\n"),
            'before_mode' => null,
            'after_mode' => '100644',
        ],
        [
            'path' => 'fixture.txt',
            'before_sha256' => hash('sha256', "original\n"),
            'after_sha256' => hash('sha256', "changed\n"),
            'before_mode' => '100644',
            'after_mode' => '100644',
        ],
    ], 'Verified changed-file metadata does not match original and snapshot bytes.');
    transport_assert(file_get_contents($root.'/workspace/sources/0/fixture.txt') === "original\n", 'Repository command modified host source.');
    $transport->stop();
    $transport->stop();
    transport_absent($name);
    fwrite(STDOUT, "PASS real Docker sibling isolation, scrubbed environment, command bridge, trusted diff and idempotent cleanup\n");
} finally {
    putenv('OPENAI_API_KEY');
    $transport->stop();
    transport_remove($root);
}

$failureMessages = [
    'oversized native output' => 'Agent sandbox output exceeds its limit.',
    'malformed bridge request' => 'Command bridge request schema is invalid.',
    'symlink bridge request' => 'Command bridge request is invalid.',
    'oversized repository output' => 'Agent sandbox output exceeds its limit.',
    'repository timeout' => 'Agent sandbox command timed out.',
];
$failurePrograms = [
    'oversized native output' => 'process.stdout.write("x".repeat(2097153)); setTimeout(() => {}, 10000);',
    'malformed bridge request' => 'fs.writeFileSync("/bridge/request.json", JSON.stringify({id:1,command:"true",extra:true})); setTimeout(() => {}, 10000);',
    'symlink bridge request' => 'fs.symlinkSync("/profile/synthetic-secret", "/bridge/request.json"); setTimeout(() => {}, 10000);',
    'oversized repository output' => '(async () => { await command(1, "head -c 65537 /dev/zero"); })();',
    'repository timeout' => '(async () => { await command(1, "sleep 30"); })();',
];
foreach ($failurePrograms as $description => $program) {
    [$root, $name, $transport] = transport_fixture($image);
    try {
        $transport->start();
        transport_rejects($description, fn () => $transport->run(['node', '-e', $client.$program], '', static function (int $requiredSeconds): void {}), $failureMessages[$description]);
        transport_absent($name);
    } finally {
        $transport->stop();
        transport_remove($root);
    }
}

[$root, $name, $transport] = transport_fixture($image, maxCommands: 1);
try {
    $transport->start();
    transport_rejects('repository command budget', fn () => $transport->run(['node', '-e', $client.<<<'JS'
(async () => { await command(1, 'true'); await command(2, 'true'); })();
JS], '', static function (int $requiredSeconds): void {}), 'Repository command budget is exhausted.');
    transport_absent($name);
} finally {
    $transport->stop();
    transport_remove($root);
}

[$root, $name, $transport] = transport_fixture($image);
try {
    $transport->start();
    transport_rejects('fresh authorization before every repository command', fn () => $transport->run(['node', '-e', $client.<<<'JS'
(async () => { await command(1, 'touch /workspace/unauthorized'); })();
JS], '', static function (int $requiredSeconds) use ($commands, $name): void {
        if ($requiredSeconds === 10) {
            $probe = $commands->run(['docker', 'exec', $name.'-repo', 'test', '!', '-e', '/workspace/unauthorized']);
            transport_assert($probe->exitCode === 0, 'Repository command ran before authorization.');
            throw new RuntimeException('Synthetic authorization was revoked.');
        }
    }), 'Synthetic authorization was revoked.');
    transport_absent($name);
} finally {
    $transport->stop();
    transport_remove($root);
}

[$root, $name, $transport] = transport_fixture($image);
try {
    $transport->start();
    $calls = 0;
    transport_rejects('checkpoint cancellation removes both trees', function () use ($transport, &$calls): void {
        $transport->run(['sh', '-c', 'sleep 30 & wait'], '', static function () use (&$calls): void {
            if (++$calls > 20) {
                throw new RuntimeException('Synthetic cancellation.');
            }
        });
    }, 'Synthetic cancellation.');
    transport_absent($name);
} finally {
    $transport->stop();
    transport_remove($root);
}

[$root, $name, $transport] = transport_fixture($image);
try {
    mkdir($root.'/workspace/sources/0/.git');
    transport_rejects('preexisting Git metadata', fn () => $transport->start(), 'Repository input must not contain Git metadata.');
    transport_absent($name);
} finally {
    $transport->stop();
    transport_remove($root);
}

[$root, $name, $transport] = transport_fixture($image);
try {
    mkdir($root.'/workspace/sources/1', 0700);
    file_put_contents($root.'/workspace/sources/1/fixture.txt', "review head\n");
    $transport->start();
    $result = $transport->run(['node', '-e', $client.<<<'JS'
(async () => { console.log(JSON.stringify(await command(1, 'cat fixture.txt /baseline/fixture.txt'))); })();
JS], '', static function (int $requiredSeconds): void {});
    $response = json_decode($result->stdout, true, flags: JSON_THROW_ON_ERROR);
    transport_assert($response['stdout'] === "review head\noriginal\n", 'Review head and base sources were inverted.');
    transport_assert($transport->patch() === null, 'Clean review head emitted a patch against the base source.');
    fwrite(STDOUT, "PASS two-source review uses head workspace and isolated base comparison\n");
} finally {
    $transport->stop();
    transport_remove($root);
}

$mutationStarted = false;
[$root, $name, $transport] = transport_fixture($image, static function (int $requiredSeconds = 0) use (&$mutationStarted): void {
    if ($requiredSeconds === 30) {
        $mutationStarted = true;
    } elseif ($mutationStarted) {
        throw new RuntimeException('Synthetic cancellation during daemon creation.');
    }
});
try {
    transport_rejects('settles canceled Docker create before confirming absence', fn () => $transport->start(), 'Synthetic cancellation during daemon creation.');
    transport_absent($name);
} finally {
    $transport->stop();
    transport_remove($root);
}

[$root, $name, $transport] = transport_fixture($image);
try {
    file_put_contents($root.'/workspace/sources/0/deleted.txt', "remove me\n");
    $transport->start();
    $result = $transport->run(['node', '-e', $client.<<<'JS'
(async () => { console.log(JSON.stringify(await command(1, 'rm deleted.txt; chmod +x fixture.txt; printf "*\\n" > .gitignore; printf "* text eol=lf\\n" > .gitattributes; printf "hidden\\r\\n" > hidden.txt; mkdir empty; printf stable > writer.txt; (while :; do printf stable > writer.next; mv writer.next writer.txt; sleep 0.02; done) >/dev/null 2>&1 &'))); })();
JS], '', static function (int $requiredSeconds): void {});
    transport_assert($result->exitCode === 0, 'Synthetic repository setup failed.');
    $patch = $transport->patch();
    $changes = array_column($transport->changedFiles(), null, 'path');
    transport_assert(isset($changes['.gitignore'], $changes['.gitattributes'], $changes['hidden.txt']), 'Repository ignore or attribute rules hid actual changes.');
    transport_assert($changes['hidden.txt']['after_sha256'] === hash('sha256', "hidden\r\n"), 'Repository attributes normalized collected bytes.');
    transport_assert($changes['deleted.txt']['before_sha256'] === hash('sha256', "remove me\n") && $changes['deleted.txt']['after_sha256'] === null, 'Deletion metadata was lost.');
    transport_assert($changes['fixture.txt']['before_mode'] === '100644' && $changes['fixture.txt']['after_mode'] === '100755', 'Executable mode change was lost.');
    transport_assert($changes['writer.txt']['after_sha256'] === hash('sha256', 'stable'), 'Frozen background-writer bytes were inconsistent.');
    transport_assert(str_contains($patch, "+hidden\r\n") && str_contains($patch, 'old mode 100644'), 'Collected diff omitted ignored bytes or file modes.');
    $repoState = json_decode($commands->mustRun(['docker', 'inspect', $name.'-repo'])->stdout, true, flags: JSON_THROW_ON_ERROR)[0];
    $collector = json_decode($commands->mustRun(['docker', 'inspect', $name.'-diff'])->stdout, true, flags: JSON_THROW_ON_ERROR)[0];
    transport_assert($repoState['State']['Paused'] === true, 'Repository writers were not frozen during collection.');
    transport_assert($collector['HostConfig']['PidMode'] === '' && $collector['HostConfig']['NetworkMode'] === 'none', 'Collector namespace isolation was widened.');
    $snapshotMount = array_values(array_filter($collector['Mounts'], static fn (array $mount): bool => $mount['Destination'] === '/snapshot-source'))[0];
    transport_assert($snapshotMount['RW'] === false && $snapshotMount['Type'] === 'volume', 'Collector acquired a writable repository mount.');
    $volume = json_decode($commands->mustRun(['docker', 'volume', 'inspect', $name.'-workspace'])->stdout, true, flags: JSON_THROW_ON_ERROR)[0];
    transport_assert($volume['Options']['type'] === 'tmpfs' && str_contains($volume['Options']['o'], 'size=256m'), 'Workspace lost its bounded memory storage.');
    fwrite(STDOUT, "PASS frozen writer collection preserves ignored files, raw bytes, deletes, modes and dotfiles\n");
} finally {
    $transport->stop();
    transport_remove($root);
}

$snapshotFailures = [
    'snapshot rejects symbolic links' => 'Archive links and special files are forbidden.',
    'snapshot rejects special files' => 'Archive links and special files are forbidden.',
    'snapshot bounds changed-file metadata' => 'Repository changed-file metadata exceeds its limit.',
];
foreach ([
    'snapshot rejects symbolic links' => 'ln -s /profile/synthetic-secret escaped-link',
    'snapshot rejects special files' => 'mkfifo fifo',
    'snapshot bounds changed-file metadata' => 'i=0; while [ "$i" -lt 201 ]; do printf changed > "file-$i"; i=$((i+1)); done',
] as $description => $command) {
    [$root, $name, $transport] = transport_fixture($image);
    try {
        $transport->start();
        $program = $client.'(async () => { console.log(JSON.stringify(await command(1, '.json_encode($command, JSON_THROW_ON_ERROR).'))); })();';
        $result = $transport->run(['node', '-e', $program], '', static function (int $requiredSeconds): void {});
        transport_assert($result->exitCode === 0, 'Synthetic snapshot adversary did not run.');
        transport_rejects($description, fn () => $transport->patch(), $snapshotFailures[$description]);
        transport_absent($name);
    } finally {
        $transport->stop();
        transport_remove($root);
    }
}

[$root, $name, $transport] = transport_fixture($image);
try {
    $transport->start();
    $longDirectory = str_repeat('nested', 30).'/'.str_repeat('nested', 30);
    $command = 'mkdir -p '.escapeshellarg($longDirectory).'; printf long > '.escapeshellarg($longDirectory.'/file.txt').'; printf option > ./-literal; printf numeric > ./123';
    $program = $client.'(async () => { console.log(JSON.stringify(await command(1, '.json_encode($command, JSON_THROW_ON_ERROR).'))); })();';
    $result = $transport->run(['node', '-e', $program], '', static function (int $requiredSeconds): void {});
    transport_assert($result->exitCode === 0, 'Synthetic long-path command failed.');
    $patch = $transport->patch();
    $changes = array_column($transport->changedFiles(), null, 'path');
    transport_assert($changes[$longDirectory.'/file.txt']['after_sha256'] === hash('sha256', 'long'), 'Long PAX path lost its verified contents.');
    transport_assert($changes['-literal']['after_sha256'] === hash('sha256', 'option') && str_contains($patch, '-literal'), 'Option-like archive name was interpreted as a command option.');
    transport_assert($changes[123]['path'] === '123' && $changes[123]['after_sha256'] === hash('sha256', 'numeric'), 'Numeric filename lost its string path in artifact metadata.');
    fwrite(STDOUT, "PASS long, option-like, and numeric filenames remain bounded snapshot data\n");
} finally {
    $transport->stop();
    transport_remove($root);
}

[$root, $name, $transport] = transport_fixture($image);
$previousContext = getenv('DOCKER_CONTEXT');
try {
    $transport->start();
    try {
        $transport->run(['node', '-e', 'process.exit(0)'], '', static function (int $requiredSeconds): void {
            putenv('DOCKER_CONTEXT=shipmunk-synthetic-unavailable-context');
            throw new RuntimeException('Synthetic original execution failure.');
        });
        throw new LogicException('Expected cleanup uncertainty.');
    } catch (RuntimeException $exception) {
        transport_assert($exception->getMessage() === 'Agent sandbox cleanup failed after execution failure.', 'Cleanup failure was not reported.');
        transport_assert($exception->getPrevious()?->getMessage() === 'Synthetic original execution failure.', 'Cleanup discarded the original execution failure.');
    }
    fwrite(STDOUT, "PASS cleanup uncertainty preserves the original execution failure\n");
} finally {
    putenv($previousContext === false ? 'DOCKER_CONTEXT' : 'DOCKER_CONTEXT='.$previousContext);
    $transport->stop();
    transport_remove($root);
}

[$root, $name, $transport] = transport_fixture($image);
try {
    file_put_contents($root.'/workspace/sources/0/group-only.txt', 'unchanged');
    chmod($root.'/workspace/sources/0/group-only.txt', 0610);
    $transport->start();
    $result = $transport->run(['node', '-e', $client.<<<'JS'
(async () => {
    const first = await command(1, 'head -c 20000 /dev/zero');
    fs.unlinkSync('/bridge/response.json');
    fs.unlinkSync('/bridge/request.json');
    const second = await command(2, 'printf recovered');
    console.log(JSON.stringify([first, second]));
})();
JS], '', static function (int $requiredSeconds): void {});
    $responses = json_decode($result->stdout, true, flags: JSON_THROW_ON_ERROR);
    transport_assert($responses === [
        [
            'id' => 1,
            'stdout' => '',
            'stderr' => 'Repository command response exceeds its frame limit.',
            'exit_code' => 1,
        ],
        [
            'id' => 2,
            'stdout' => 'recovered',
            'stderr' => '',
            'exit_code' => 0,
        ],
    ], 'Completed command frame overflow did not return a bounded error and preserve sequencing.');
    transport_assert($transport->patch() === null && $transport->changedFiles() === [], 'Group-only execution mode created a false Git mode change.');
    fwrite(STDOUT, "PASS completed response overflow permits the next command and preserves Git owner modes\n");
} finally {
    $transport->stop();
    transport_remove($root);
}

$revoked = false;
[$root, $name, $transport] = transport_fixture($image, static function (int $requiredSeconds) use (&$revoked): void {
    if ($revoked) {
        throw new RuntimeException('Synthetic persistent authorization revocation.');
    }
});
try {
    $transport->start();
    $revoked = true;
    transport_rejects('mandatory cleanup ignores revoked execution authorization', fn () => $transport->patch(), 'Synthetic persistent authorization revocation.');
    transport_absent($name);
} finally {
    $transport->stop();
    transport_remove($root);
}

foreach (['slow-git', 'unsupported-tar'] as $variant) {
    $build = realpath(sys_get_temp_dir()).'/shipmunk-transport-image-'.bin2hex(random_bytes(8));
    mkdir($build, 0700);
    $fixtureImage = 'shipmunk-transport-review-'.bin2hex(random_bytes(8)).':local';
    $tool = $variant === 'slow-git' ? 'git' : 'tar';
    $script = $variant === 'slow-git' ? <<<'SH'
#!/bin/sh
if [ "$GIT_DIR" = /git ]; then
    case " $* " in
        *" add -N "*|*" diff "*) sleep 11 ;;
    esac
fi
exec /usr/bin/git "$@"
SH : <<<'SH'
#!/bin/sh
printf 'SYNTHETIC_ACCOUNT_SECRET unsupported archive flags' >&2
exit 2
SH;
    file_put_contents($build.'/'.$tool, $script."\n");
    file_put_contents($build.'/Dockerfile', "ARG BASE\nFROM \${BASE}\nUSER root\nCOPY --chmod=755 {$tool} /usr/local/bin/{$tool}\n");
    try {
        $commands->mustRun(['docker', 'build', '--pull=false', '--build-arg', 'BASE='.$image, '-t', $fixtureImage, $build], timeoutSeconds: 120);
        $checkpoints = 0;
        [$root, $name, $transport] = transport_fixture($fixtureImage, static function (int $requiredSeconds) use (&$checkpoints): void {
            $checkpoints++;
        });
        try {
            if ($variant === 'unsupported-tar') {
                transport_rejects('unsupported archive image fails during startup', fn () => $transport->start(), 'Repository image lacks required archive capabilities.');
                transport_absent($name);
            } else {
                $transport->start();
                $transport->run(['node', '-e', $client.'(async () => { await command(1, "printf changed > fixture.txt"); })();'], '', static function (int $requiredSeconds): void {});
                $checkpoints = 0;
                $patch = $transport->patch();
                transport_assert(str_contains($patch, '+changed'), 'Collection rejected valid Git work beyond the repository-command deadline.');
                transport_assert($checkpoints > 100, 'Long collection stopped polling execution authorization.');
                fwrite(STDOUT, "PASS slow Git collection retains its longer deadline and checkpoints\n");
            }
        } finally {
            $transport->stop();
            transport_remove($root);
        }
    } finally {
        $commands->run(['docker', 'image', 'rm', $fixtureImage]);
        transport_remove($build);
    }
}
