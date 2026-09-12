<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Drivers;

use Closure;
use FilesystemIterator;
use RecursiveDirectoryIterator;
use RecursiveIteratorIterator;
use RuntimeException;
use Shipmunk\Runner\CommandResult;
use Shipmunk\Runner\SafeTarExtractor;
use Throwable;

/**
 * Native credentials and repository programs occupy separate containers.
 */
final class DockerAgentTransport implements AgentTransport
{
    private bool $started = false;

    private int $lastRequest = 0;

    private bool $unsettledMutation = false;

    private string $bridge;

    private string $selectedSource = '';

    private string $repositoryId = '';

    private bool $frozen = false;

    private bool $patchCollected = false;

    private ?string $collectedPatch = null;

    /**
     * @var list<array<string, string|null>>
     */
    private array $verifiedChanges = [];

    /**
     * @param  Closure(int): void  $checkpoint
     */
    public function __construct(
        private readonly string $name,
        private readonly string $home,
        private readonly string $workspace,
        private readonly string $nativeImage,
        private readonly string $repositoryImage,
        private readonly Closure $checkpoint,
        private readonly int $maxCommands = 100,
        private readonly ?int $sourceIndex = null,
    ) {
        if ($maxCommands < 1 || preg_match('/^shipmunk-codex-[0-7][0-9a-hjkmnp-tv-z]{25}-[1-9][0-9]*$/D', $name) !== 1) {
            throw new RuntimeException('Invalid agent sandbox name.');
        }
        $this->bridge = $workspace.'/bridge';
    }

    public function start(): void
    {
        if ($this->started || posix_geteuid() === 0) {
            throw new RuntimeException('A fresh sandbox and dedicated non-root runner are required.');
        }
        $sourceEntries = @scandir($this->workspace.'/sources');
        $sources = is_array($sourceEntries) ? array_values(array_diff($sourceEntries, ['.', '..'])) : [];
        if (! in_array($sources, [['0'], ['0', '1']], true)) {
            throw new RuntimeException('Repository source layout is invalid.');
        }
        $selected = $this->sourceIndex ?? (count($sources) === 2 ? 1 : 0);
        if (! in_array($selected, [0, 1], true) || ! in_array((string) $selected, $sources, true)) {
            throw new RuntimeException('Repository source selection is invalid.');
        }
        $sourceDirectories = array_map(fn (string $index): string => $this->workspace.'/sources/'.$index, $sources);
        foreach ([$this->home, $this->workspace, ...$sourceDirectories] as $directory) {
            if (realpath($directory) !== $directory || ! is_dir($directory)) {
                throw new RuntimeException('Sandbox input directory is invalid.');
            }
        }
        if (str_contains($this->home, ',') || str_contains($this->workspace, ',')) {
            throw new RuntimeException('Sandbox mount path is invalid.');
        }
        foreach ($sourceDirectories as $directory) {
            if (@lstat($directory.'/.git') !== false) {
                throw new RuntimeException('Repository input must not contain Git metadata.');
            }
        }
        if (! @mkdir($this->bridge, 0700)) {
            throw new RuntimeException('Cannot create a fresh protected command bridge.');
        }
        $this->validateBridgeDirectory();

        try {
            $nativeId = $this->imageId($this->nativeImage);
            $repositoryId = $this->imageId($this->repositoryImage);
            $this->repositoryId = $repositoryId;
            $this->selectedSource = $this->workspace.'/sources/'.$selected;
            $uid = posix_geteuid();
            $gid = posix_getegid();
            $this->mustRun([
                'docker', 'volume', 'create', '--driver', 'local',
                '--opt', 'type=tmpfs', '--opt', 'device=tmpfs',
                '--opt', 'o=size=256m,uid='.$uid.',gid='.$gid.',mode=0700,nosuid,nodev',
                $this->name.'-workspace',
            ]);
            $common = [
                '--init', '--read-only', '--user', $uid.':'.$gid,
                '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true',
                '--memory', '512m', '--memory-swap', '512m', '--cpus', '1',
                '--ulimit', 'nofile=1024:1024', '--log-driver', 'none', '--stop-timeout', '2',
            ];
            $this->mustRun([
                'docker', 'create', '--name', $this->name, ...$common,
                '--pids-limit', '128',
                '--network', 'bridge', '--workdir', '/empty',
                '--tmpfs', '/tmp:rw,nosuid,nodev,noexec,size=64m,mode=1777',
                '--mount', 'type=bind,src='.$this->home.',dst=/profile',
                '--mount', 'type=bind,src='.$this->bridge.',dst=/bridge',
                '--tmpfs', '/profile/.codex/tmp:rw,nosuid,nodev,size=16m,mode=0700,uid='.$uid.',gid='.$gid,
                '--entrypoint', '/usr/bin/env', $nativeId,
                '-i', 'PATH=/usr/local/bin:/usr/bin:/bin', '/bin/sh', '-c',
                'test ! -e /etc/claude-code && test ! -e /etc/codex && exec sleep 1800',
            ]);
            $this->mustRun([
                'docker', 'create', '--name', $this->name.'-repo', ...$common,
                '--pids-limit', '64',
                '--network', 'none', '--workdir', '/workspace',
                '--mount', 'type=volume,src='.$this->name.'-workspace,dst=/workspace',
                '--tmpfs', '/baseline:rw,noexec,nosuid,nodev,size=128m,mode=0700,uid='.$uid.',gid='.$gid,
                '--tmpfs', '/tmp:rw,nosuid,nodev,noexec,size=64m,mode=1777',
                '--entrypoint', '/usr/bin/env', $repositoryId,
                '-i', 'PATH=/usr/local/bin:/usr/bin:/bin', '/bin/sleep', '1800',
            ]);
            $this->mustRun(['docker', 'start', $this->name]);
            $this->mustRun(['docker', 'start', $this->name.'-repo']);
            foreach ([
                ['tar', '--format=pax', '--pax-option=exthdr.name=PaxHeader,delete=atime,delete=ctime,delete=mtime',
                    '--no-recursion', '--null', '--verbatim-files-from', '-cf', '/dev/null', '-T', '/dev/null'],
                ['find', '/tmp', '-maxdepth', '0', '-printf', ''],
            ] as $probe) {
                ($this->checkpoint)(10);
                $result = $this->execute($this->repositoryCommand($probe), '', 10, 65_536, $this->checkpoint);
                if ($result->exitCode !== 0) {
                    throw new RuntimeException('Repository image lacks required archive capabilities.');
                }
            }
            // Archive bytes are data: only the isolated repository container extracts them.
            $archive = $this->mustRun(['/usr/bin/env', '-i', 'PATH=/usr/local/bin:/usr/bin:/bin', 'COPYFILE_DISABLE=1', 'tar', '-C', $this->workspace.'/sources/'.$selected, '-cf', '-', '.'], maxBytes: 67_108_864, timeoutSeconds: 120)->stdout;
            $this->mustRun($this->repositoryCommand(['tar', '-xf', '-', '-C', '/workspace']), $archive, timeoutSeconds: 120);
            if (count($sources) === 2) {
                $baseline = $this->mustRun(['/usr/bin/env', '-i', 'PATH=/usr/local/bin:/usr/bin:/bin', 'COPYFILE_DISABLE=1', 'tar', '-C', $this->workspace.'/sources/0', '-cf', '-', '.'], maxBytes: 67_108_864, timeoutSeconds: 120)->stdout;
                $this->mustRun($this->repositoryCommand(['tar', '-xf', '-', '-C', '/baseline']), $baseline, timeoutSeconds: 120);
            }
            $this->mustRun($this->repositoryCommand([
                'sh', '-c', 'test ! -e .git && test ! -L .git'
                    .' && git -c core.hooksPath=/dev/null init -q'
                    .' && git -c core.hooksPath=/dev/null add --all'
                    .' && git -c core.hooksPath=/dev/null -c user.name=Shipmunk -c user.email=runner@shipmunk.local commit -qm baseline --allow-empty',
            ]));
            $this->started = true;
        } catch (Throwable $exception) {
            $this->cleanupAfterFailure($exception);
        }
    }

    /**
     * @param  list<string>  $argv
     * @param  Closure(int): void  $checkpoint
     */
    public function run(
        array $argv,
        string $stdin,
        Closure $checkpoint,
    ): CommandResult {
        if (! $this->started || $argv === [] || ! array_is_list($argv)) {
            throw new RuntimeException('Agent sandbox is not ready.');
        }
        foreach ($argv as $argument) {
            if (! is_string($argument) || str_contains($argument, "\0")) {
                throw new RuntimeException('Agent command arguments are invalid.');
            }
        }
        try {
            $checkpoint(0);

            $result = $this->execute([
                'docker', 'exec', '-i', $this->name, '/usr/bin/env', '-i',
                'HOME=/profile', 'CODEX_HOME=/profile/.codex',
                'XDG_CONFIG_HOME=/profile/.config', 'XDG_CACHE_HOME=/profile/.cache',
                'PATH=/usr/local/bin:/usr/bin:/bin', 'LANG=C.UTF-8', 'TERM=dumb',
                '/bin/sh', '-c', 'umask 077; exec "$@"', 'native-agent', ...$argv,
            ], $stdin, 1800, 2_097_152, $checkpoint, true);
            if ($result->exitCode !== 0) {
                $this->stop();
            }

            return $result;
        } catch (Throwable $exception) {
            $this->cleanupAfterFailure($exception);
        }
    }

    public function patch(): ?string
    {
        if (! $this->started) {
            throw new RuntimeException('Repository sandbox is not ready.');
        }
        if ($this->patchCollected) {
            return $this->collectedPatch;
        }

        try {
            // Neither the repository's Git metadata nor its background writers are trusted.
            $this->mustRun(['docker', 'pause', $this->name.'-repo']);
            $paused = $this->mustRun(['docker', 'inspect', '--format', '{{.State.Paused}}', $this->name.'-repo']);
            if (trim($paused->stdout) !== 'true') {
                throw new RuntimeException('Cannot freeze repository writers for collection.');
            }
            $this->frozen = true;
            $this->startCollector();
            $entries = $this->mustRun($this->collectorCommand([
                'find', '/snapshot-source', '-mindepth', '1', '-name', '.git', '-prune',
                '-o', '-mindepth', '1', '-printf', '%P\\0',
            ]), maxBytes: 20_971_520, timeoutSeconds: 120)->stdout;
            if (substr_count($entries, "\0") > 20_000) {
                throw new RuntimeException('Repository snapshot entry count exceeds its limit.');
            }
            $archive = $this->mustRun($this->collectorCommand([
                'tar', '--format=pax', '--pax-option=exthdr.name=PaxHeader,delete=atime,delete=ctime,delete=mtime',
                '--no-recursion', '--null', '--verbatim-files-from',
                '-cf', '-', '-C', '/snapshot-source', '-T', '-',
            ]), $entries, maxBytes: 134_217_728, timeoutSeconds: 120)->stdout;
            $snapshot = $this->workspace.'/snapshot';
            if (! @mkdir($snapshot, 0700)) {
                throw new RuntimeException('Cannot create a fresh repository snapshot.');
            }
            (new SafeTarExtractor)->extract($archive, $snapshot, $this->checkpoint);
            $this->verifiedChanges = $this->compareSnapshot($snapshot);
            $original = $this->mustRun(['/usr/bin/env', '-i', 'PATH=/usr/local/bin:/usr/bin:/bin', 'COPYFILE_DISABLE=1', 'tar', '-C', $this->selectedSource, '-cf', '-', '.'], maxBytes: 67_108_864, timeoutSeconds: 120)->stdout;
            $this->mustRun($this->collectorCommand(['tar', '-xf', '-', '-C', '/workspace']), $original, timeoutSeconds: 120);
            $this->mustRun($this->collectorCommand([
                'sh', '-c', 'git -c core.hooksPath=/dev/null init -q'
                    ." && printf '%s\\n' '** -text -filter -ident -working-tree-encoding !eol !diff' > /git/info/attributes"
                    .' && git -c core.hooksPath=/dev/null add --all --force'
                    .' && git -c core.hooksPath=/dev/null -c user.name=Shipmunk -c user.email=runner@shipmunk.local commit -qm baseline --allow-empty'
                    .' && rm -rf /workspace/* /workspace/.[!.]* /workspace/..?*',
            ]), timeoutSeconds: 120);
            $safeSnapshot = $this->mustRun(['/usr/bin/env', '-i', 'PATH=/usr/local/bin:/usr/bin:/bin', 'COPYFILE_DISABLE=1', 'tar', '-C', $snapshot, '-cf', '-', '.'], maxBytes: 134_217_728, timeoutSeconds: 120)->stdout;
            $this->mustRun($this->collectorCommand(['tar', '-xf', '-', '-C', '/workspace']), $safeSnapshot, timeoutSeconds: 120);
            $this->mustRun($this->collectorCommand(['git', '-c', 'core.hooksPath=/dev/null', 'add', '-N', '--force', '.']), timeoutSeconds: 120);
            $patch = $this->mustRun($this->collectorCommand([
                'git', '-c', 'core.hooksPath=/dev/null', '-c', 'diff.external=',
                'diff', '--no-ext-diff', '--no-textconv', '--binary', 'HEAD', '--', '.',
            ]), maxBytes: 1_048_576, timeoutSeconds: 120)->stdout;
            if (($patch === '') !== ($this->verifiedChanges === [])) {
                throw new RuntimeException('Collected diff does not represent the verified file changes.');
            }
            $this->collectedPatch = $patch === '' ? null : $patch;
            $this->patchCollected = true;

            return $this->collectedPatch;
        } catch (Throwable $exception) {
            $this->cleanupAfterFailure($exception);
        }
    }

    /**
     * @return list<array<string, string|null>>
     */
    public function changedFiles(): array
    {
        if (! $this->patchCollected) {
            throw new RuntimeException('Verified snapshot metadata is not available.');
        }

        return $this->verifiedChanges;
    }

    /**
     * @return list<array<string, string|null>>
     */
    private function compareSnapshot(string $snapshot): array
    {
        $before = $this->files($this->selectedSource);
        $after = $this->files($snapshot);
        $paths = array_unique([...array_keys($before), ...array_keys($after)]);
        sort($paths);
        $changes = [];
        foreach ($paths as $path) {
            if (($before[$path] ?? null) === ($after[$path] ?? null)) {
                continue;
            }
            $changes[] = [
                'path' => (string) $path,
                'before_sha256' => $before[$path]['sha256'] ?? null,
                'after_sha256' => $after[$path]['sha256'] ?? null,
                'before_mode' => $before[$path]['mode'] ?? null,
                'after_mode' => $after[$path]['mode'] ?? null,
            ];
            if (count($changes) > 200) {
                throw new RuntimeException('Repository changed-file metadata exceeds its limit.');
            }
        }

        return $changes;
    }

    /**
     * @return array<string, array{sha256: string, mode: string}>
     */
    private function files(string $directory): array
    {
        $files = [];
        foreach (new RecursiveIteratorIterator(new RecursiveDirectoryIterator($directory, FilesystemIterator::SKIP_DOTS)) as $entry) {
            ($this->checkpoint)(0);
            if ($entry->isDir() && ! $entry->isLink()) {
                continue;
            }
            $path = substr($entry->getPathname(), strlen($directory) + 1);
            $stat = @lstat($entry->getPathname());
            if ($stat === false || ($stat['mode'] & 0170000) !== 0100000 || $stat['nlink'] !== 1
                || strlen($path) > 1024 || preg_match('~^(?!/)(?!.*(?:^|/)\.\.(?:/|$))(?!.*\\\\)[^\x00-\x1f]+$~uD', $path) !== 1
                || in_array('.git', explode('/', $path), true)) {
                throw new RuntimeException('Repository snapshot file metadata is invalid.');
            }
            $hash = hash_file('sha256', $entry->getPathname());
            if ($hash === false) {
                throw new RuntimeException('Cannot hash repository snapshot file.');
            }
            $files[$path] = [
                'sha256' => $hash,
                'mode' => ($stat['mode'] & 0100) !== 0 ? '100755' : '100644',
            ];
        }

        return $files;
    }

    private function startCollector(): void
    {
        $uid = posix_geteuid();
        $gid = posix_getegid();
        $this->mustRun([
            'docker', 'create', '--name', $this->name.'-diff',
            '--init', '--read-only', '--user', $uid.':'.$gid,
            '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true',
            '--pids-limit', '64', '--memory', '512m', '--memory-swap', '512m', '--cpus', '1',
            '--ulimit', 'nofile=1024:1024', '--log-driver', 'none', '--stop-timeout', '2',
            '--network', 'none', '--workdir', '/workspace',
            '--mount', 'type=volume,src='.$this->name.'-workspace,dst=/snapshot-source,readonly',
            '--tmpfs', '/workspace:rw,noexec,nosuid,nodev,size=256m,mode=0700,uid='.$uid.',gid='.$gid,
            '--tmpfs', '/git:rw,noexec,nosuid,nodev,size=64m,mode=0700,uid='.$uid.',gid='.$gid,
            '--tmpfs', '/tmp:rw,noexec,nosuid,nodev,size=16m,mode=1777',
            '--entrypoint', '/usr/bin/env', $this->repositoryId,
            '-i', 'PATH=/usr/local/bin:/usr/bin:/bin', '/bin/sleep', '1800',
        ]);
        $this->mustRun(['docker', 'start', $this->name.'-diff']);
    }

    /**
     * @param  list<string>  $argv
     * @return list<string>
     */
    private function collectorCommand(array $argv): array
    {
        return [
            'docker', 'exec', '-i', $this->name.'-diff', '/usr/bin/env', '-i',
            'HOME=/nonexistent', 'PATH=/usr/local/bin:/usr/bin:/bin', 'LANG=C.UTF-8', 'TERM=dumb',
            'GIT_CONFIG_NOSYSTEM=1', 'GIT_CONFIG_GLOBAL=/dev/null',
            'GIT_DIR=/git', 'GIT_WORK_TREE=/workspace',
            ...$argv,
        ];
    }

    private function cleanupAfterFailure(Throwable $exception): never
    {
        try {
            $this->stop();
        } catch (Throwable) {
            throw new RuntimeException('Agent sandbox cleanup failed after execution failure.', previous: $exception);
        }

        throw $exception;
    }

    public function stop(): void
    {
        $failed = false;
        // Each sibling is reconciled even if inspecting or removing the other fails.
        foreach ([$this->name, $this->name.'-repo', $this->name.'-diff'] as $name) {
            try {
                $inspect = $this->execute(['docker', 'inspect', $name], '', 10, 65_536);
                if ($inspect->exitCode === 0) {
                    foreach ([['docker', 'stop', '--time', '2', $name], ['docker', 'rm', '--force', $name]] as $command) {
                        try {
                            $this->execute($command, '', 10, 65_536);
                        } catch (Throwable) {
                            // Continue to forced removal and final absence confirmation.
                        }
                    }
                    $inspect = $this->execute(['docker', 'inspect', $name], '', 10, 65_536);
                }
                if ($inspect->exitCode === 0 || preg_match('/no such (?:object|container)/i', $inspect->stdout.$inspect->stderr) !== 1) {
                    $failed = true;
                }
            } catch (Throwable) {
                $failed = true;
            }
        }
        $this->started = false;
        if (! $failed && ! $this->unsettledMutation) {
            try {
                $volume = $this->execute(['docker', 'volume', 'inspect', $this->name.'-workspace'], '', 10, 65_536);
                if ($volume->exitCode === 0) {
                    $this->execute(['docker', 'volume', 'rm', $this->name.'-workspace'], '', 10, 65_536);
                    $volume = $this->execute(['docker', 'volume', 'inspect', $this->name.'-workspace'], '', 10, 65_536);
                }
                if ($volume->exitCode === 0 || preg_match('/no such volume/i', $volume->stdout.$volume->stderr) !== 1) {
                    $failed = true;
                }
            } catch (Throwable) {
                $failed = true;
            }
        }
        if ($failed || $this->unsettledMutation) {
            throw new RuntimeException('Cannot confirm all agent process trees are absent.');
        }
    }

    private function imageId(string $image): string
    {
        $id = trim($this->mustRun(['docker', 'image', 'inspect', '--format', '{{.Id}}', $image])->stdout);
        if (preg_match('/^sha256:[a-f0-9]{64}$/D', $id) !== 1) {
            throw new RuntimeException('A preloaded immutable agent image is required.');
        }

        return $id;
    }

    /**
     * @param  list<string>  $argv
     * @return list<string>
     */
    private function repositoryCommand(array $argv): array
    {
        return [
            'docker', 'exec', '-i', $this->name.'-repo', '/usr/bin/env', '-i',
            'HOME=/workspace', 'PATH=/usr/local/bin:/usr/bin:/bin', 'LANG=C.UTF-8', 'TERM=dumb',
            'GIT_CONFIG_NOSYSTEM=1', 'GIT_CONFIG_GLOBAL=/dev/null',
            ...$argv,
        ];
    }

    private function validateBridgeDirectory(): void
    {
        clearstatcache(true, $this->bridge);
        $stat = @lstat($this->bridge);
        if ($stat === false || ($stat['mode'] & 0170000) !== 0040000 || ($stat['mode'] & 0077) !== 0 || $stat['uid'] !== posix_geteuid()) {
            throw new RuntimeException('Command bridge directory is not protected.');
        }
    }

    /**
     * @param  Closure(int): void  $checkpoint
     */
    private function bridge(Closure $checkpoint): void
    {
        $this->validateBridgeDirectory();
        $path = $this->bridge.'/request.json';
        clearstatcache(true, $path);
        $stat = @lstat($path);
        if ($stat === false) {
            return;
        }
        if (($stat['mode'] & 0170000) !== 0100000 || $stat['uid'] !== posix_geteuid() || ($stat['mode'] & 0077) !== 0 || $stat['nlink'] !== 1 || $stat['size'] > 65_536) {
            throw new RuntimeException('Command bridge request is invalid.');
        }
        $handle = @fopen($path, 'rb');
        if ($handle === false) {
            throw new RuntimeException('Cannot read command bridge request.');
        }
        try {
            $opened = fstat($handle);
            if ($opened === false || $opened['dev'] !== $stat['dev'] || $opened['ino'] !== $stat['ino'] || $opened['nlink'] !== 1) {
                throw new RuntimeException('Command bridge request changed while opening.');
            }
            $bytes = stream_get_contents($handle, 65_537);
        } finally {
            fclose($handle);
        }
        if (! is_string($bytes) || strlen($bytes) > 65_536) {
            throw new RuntimeException('Command bridge request exceeds its limit.');
        }
        // Property tokens distinguish duplicate keys from colons inside command text.
        preg_match_all('/"(?:[^"\\\\]|\\\\.)*"|[{}\[\]:,]/s', $bytes, $tokens);
        if (count(array_filter($tokens[0], static fn (string $token): bool => $token === ':')) !== 2) {
            throw new RuntimeException('Command bridge request schema is invalid.');
        }
        $request = json_decode($bytes, true, 4);
        $keys = is_array($request) ? array_keys($request) : [];
        sort($keys);
        if (! is_array($request) || $keys !== ['command', 'id']
            || ! is_int($request['id']) || $request['id'] < 1 || $request['id'] > 9_007_199_254_740_991
            || ! is_string($request['command']) || $request['command'] === '' || strlen($request['command']) > 8192 || str_contains($request['command'], "\0")) {
            throw new RuntimeException('Command bridge request schema is invalid.');
        }
        if ($request['id'] === $this->lastRequest) {
            return;
        }
        if ($request['id'] !== $this->lastRequest + 1) {
            throw new RuntimeException('Command bridge request sequence is invalid.');
        }
        if ($this->frozen) {
            throw new RuntimeException('Repository snapshot is sealed against further commands.');
        }
        if ($this->lastRequest >= $this->maxCommands) {
            throw new RuntimeException('Repository command budget is exhausted.');
        }
        $checkpoint(10);
        $result = $this->execute($this->repositoryCommand(['sh', '-c', $request['command']]), '', 10, 131_072, $checkpoint, false, 65_536);
        $this->lastRequest = $request['id'];
        $response = json_encode([
            'id' => $request['id'],
            'stdout' => $result->stdout,
            'stderr' => $result->stderr,
            'exit_code' => $result->exitCode,
        ], JSON_THROW_ON_ERROR | JSON_INVALID_UTF8_SUBSTITUTE);
        if (strlen($response) > 65_536) {
            // Execution has exited; only its encoded response is too large. No live
            // command is resumed after a timeout or output-stream limit failure.
            $response = json_encode([
                'id' => $request['id'],
                'stdout' => '',
                'stderr' => 'Repository command response exceeds its frame limit.',
                'exit_code' => 1,
            ], JSON_THROW_ON_ERROR);
        }
        $temporary = $this->bridge.'/.response-'.bin2hex(random_bytes(12));
        $handle = @fopen($temporary, 'xb');
        if ($handle === false) {
            throw new RuntimeException('Cannot create command bridge response.');
        }
        try {
            if (! chmod($temporary, 0600) || fwrite($handle, $response) !== strlen($response) || ! fflush($handle)) {
                throw new RuntimeException('Cannot write command bridge response.');
            }
        } finally {
            fclose($handle);
        }
        if (! @rename($temporary, $this->bridge.'/response.json')) {
            @unlink($temporary);
            throw new RuntimeException('Cannot publish command bridge response.');
        }
    }

    /**
     * @param  list<string>  $argv
     */
    private function mustRun(
        array $argv,
        string $stdin = '',
        int $maxBytes = 65_536,
        int $timeoutSeconds = 10,
    ): CommandResult {
        $settle = $argv[0] === 'docker'
            && (in_array($argv[1] ?? null, ['create', 'start', 'pause'], true)
                || (($argv[1] ?? null) === 'volume' && ($argv[2] ?? null) === 'create'));
        $timeout = $settle ? max(30, $timeoutSeconds) : $timeoutSeconds;
        // The subprocess can outlive one lease because polling renews it continuously.
        ($this->checkpoint)(min($timeout, 30));
        $result = $this->execute($argv, $stdin, $timeout, $maxBytes, $this->checkpoint, settle: $settle);
        if ($result->exitCode !== 0) {
            throw new RuntimeException('Agent sandbox command failed.');
        }

        return $result;
    }

    /**
     * @param  list<string>  $argv
     * @param  (Closure(int): void)|null  $checkpoint
     */
    private function execute(
        array $argv,
        string $stdin,
        int $timeout,
        int $maxBytes,
        ?Closure $checkpoint = null,
        bool $bridge = false,
        ?int $maxStreamBytes = null,
        bool $settle = false,
    ): CommandResult {
        $pipes = [];
        $process = @proc_open($argv, [
            0 => ['pipe', 'r'],
            1 => ['pipe', 'w'],
            2 => ['pipe', 'w'],
        ], $pipes);
        if (! is_resource($process)) {
            throw new RuntimeException('Cannot start agent sandbox command.');
        }
        foreach ($pipes as $pipe) {
            stream_set_blocking($pipe, false);
        }
        $deadline = hrtime(true) + $timeout * 1_000_000_000;
        $stdout = '';
        $stderr = '';
        $written = 0;
        $deferredFailure = null;
        $nextCheckpoint = 0;
        $nextBridge = 0;
        try {
            do {
                if ($deferredFailure === null) {
                    try {
                        if (hrtime(true) >= $nextCheckpoint) {
                            $checkpoint?->__invoke(0);
                            $nextCheckpoint = hrtime(true) + 50_000_000;
                        }
                        if (hrtime(true) >= $deadline) {
                            throw new RuntimeException('Agent sandbox command timed out.');
                        }
                    } catch (Throwable $exception) {
                        if (! $settle) {
                            throw $exception;
                        }
                        // Docker may finish creating or starting after its client is killed.
                        // Await the daemon response before cleanup can establish absence.
                        $deferredFailure = $exception;
                        $deadline = hrtime(true) + 60 * 1_000_000_000;
                    }
                } elseif (hrtime(true) >= $deadline) {
                    $this->unsettledMutation = true;
                    throw new RuntimeException('Agent sandbox creation could not be settled.');
                }
                if (is_resource($pipes[0])) {
                    if ($written >= strlen($stdin)) {
                        fclose($pipes[0]);
                    } else {
                        $count = @fwrite($pipes[0], substr($stdin, $written, 65_536));
                        if ($count === false) {
                            throw new RuntimeException('Cannot write agent sandbox input.');
                        }
                        $written += $count;
                    }
                }
                $stdout .= (string) fread($pipes[1], 65_536);
                $stderr .= (string) fread($pipes[2], 65_536);
                $this->validateOutput($stdout, $stderr, $maxBytes, $maxStreamBytes);
                if ($bridge && hrtime(true) >= $nextBridge) {
                    $this->bridge($checkpoint);
                    $nextBridge = hrtime(true) + 50_000_000;
                }
                $status = proc_get_status($process);
                if ($status['running']) {
                    $read = [$pipes[1], $pipes[2]];
                    $write = is_resource($pipes[0]) ? [$pipes[0]] : [];
                    $except = null;
                    @stream_select($read, $write, $except, 0, 50_000);
                }
            } while ($status['running']);
            // Drain only bounded chunks after exit; stream_get_contents without a limit is unsafe.
            foreach ([1, 2] as $index) {
                while (! feof($pipes[$index])) {
                    $bytes = fread($pipes[$index], 65_536);
                    if ($bytes === false || $bytes === '') {
                        break;
                    }
                    if ($index === 1) {
                        $stdout .= $bytes;
                    } else {
                        $stderr .= $bytes;
                    }
                    $this->validateOutput($stdout, $stderr, $maxBytes, $maxStreamBytes);
                }
            }

            if ($deferredFailure !== null) {
                throw $deferredFailure;
            }

            return new CommandResult($status['exitcode'], $stdout, $stderr);
        } finally {
            if (proc_get_status($process)['running']) {
                if ($settle) {
                    $this->unsettledMutation = true;
                }
                proc_terminate($process, 9);
            }
            foreach ($pipes as $pipe) {
                if (is_resource($pipe)) {
                    fclose($pipe);
                }
            }
            proc_close($process);
        }
    }

    private function validateOutput(
        string $stdout,
        string $stderr,
        int $maxBytes,
        ?int $maxStreamBytes,
    ): void {
        if (strlen($stdout) + strlen($stderr) > $maxBytes
            || ($maxStreamBytes !== null && (strlen($stdout) > $maxStreamBytes || strlen($stderr) > $maxStreamBytes))) {
            throw new RuntimeException('Agent sandbox output exceeds its limit.');
        }
    }
}
