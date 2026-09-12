<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Profiles;

use Closure;
use RuntimeException;
use Shipmunk\Runner\CommandResult;
use Shipmunk\Runner\CommandRunner;

/** Credential-only Linux container; never accepts repository commands or mounts. */
final readonly class DockerProfileRuntime implements ProfileRuntime
{
    public function __construct(private string $image, private CommandRunner $commands = new CommandRunner) {}

    public function start(string $sandbox, string $home): void
    {
        $image = $this->commands->mustRun(['docker', 'image', 'inspect', '--format', '{{.Id}}', $this->image]);
        $id = trim($image->stdout);
        if (preg_match('/^sha256:[a-f0-9]{64}$/D', $id) !== 1 || posix_geteuid() === 0) {
            throw new RuntimeException('A preloaded image and dedicated non-root runner account are required.');
        }
        $this->commands->mustRun([
            'docker', 'create', '--name', $sandbox, '--init', '--read-only',
            '--user', posix_geteuid().':'.posix_getegid(),
            '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
            '--pids-limit', '64', '--memory', '512m', '--cpus', '1',
            '--ulimit', 'nofile=1024:1024', '--log-driver', 'none',
            '--network', 'bridge', '--workdir', '/empty',
            '--tmpfs', '/tmp:rw,nosuid,nodev,noexec,size=64m,mode=1777',
            '--mount', 'type=bind,src='.$home.',dst=/profile',
            '--tmpfs', '/profile/.codex/tmp:rw,nosuid,nodev,size=16m,mode=0700,uid='.posix_geteuid().',gid='.posix_getegid(),
            '--entrypoint', '/usr/bin/env', $id,
            '-i', 'PATH=/usr/local/bin:/usr/bin:/bin', '/bin/sh', '-c',
            'test ! -e /etc/claude-code && test ! -e /etc/codex && exec sleep 1800',
        ]);
        $this->commands->mustRun(['docker', 'start', $sandbox]);
    }

    public function run(string $sandbox, string $agent, string $command, Closure $checkpoint): CommandResult
    {
        $checkpoint();
        $argv = [
            'docker', 'exec', '-i', $sandbox, '/usr/bin/env', '-i',
            'HOME=/profile', 'CODEX_HOME=/profile/.codex', 'CLAUDE_CONFIG_DIR=/profile/.claude',
            'XDG_CONFIG_HOME=/profile/.config', 'XDG_CACHE_HOME=/profile/.cache',
            'PATH=/usr/local/bin:/usr/bin:/bin', 'LANG=C.UTF-8', 'TERM=dumb',
            'DISABLE_AUTOUPDATER=1', 'CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1',
            '/bin/sh', '-c', 'umask 077; exec "$@"', 'native-profile',
            ...NativeProfile::command($agent, $command),
        ];
        $interactive = $command === 'login';
        $pipes = [];
        $process = proc_open($argv, [
            0 => $interactive ? STDIN : ['file', '/dev/null', 'r'],
            1 => $interactive ? STDOUT : ['pipe', 'w'],
            2 => $interactive ? STDERR : ['pipe', 'w'],
        ], $pipes);
        if (! is_resource($process)) {
            throw new RuntimeException('Cannot start native profile command.');
        }
        foreach ($pipes as $pipe) {
            stream_set_blocking($pipe, false);
        }
        $stdout = '';
        $stderr = '';
        $deadline = hrtime(true) + (($interactive ? 900 : 30) * 1_000_000_000);
        try {
            do {
                $checkpoint();
                if (hrtime(true) >= $deadline) {
                    throw new RuntimeException('Native profile command timed out.');
                }
                if (! $interactive) {
                    $stdout .= (string) stream_get_contents($pipes[1]);
                    $stderr .= (string) stream_get_contents($pipes[2]);
                    if (strlen($stdout) + strlen($stderr) > 16_384) {
                        throw new RuntimeException('Native profile output exceeded its limit.');
                    }
                }
                $status = proc_get_status($process);
                if ($status['running']) {
                    usleep(50_000);
                }
            } while ($status['running']);
            if (! $interactive) {
                $stdout .= (string) stream_get_contents($pipes[1]);
                $stderr .= (string) stream_get_contents($pipes[2]);
            }
            if (strlen($stdout) + strlen($stderr) > 16_384) {
                throw new RuntimeException('Native profile output exceeded its limit.');
            }

            return new CommandResult($status['exitcode'], $stdout, $stderr);
        } finally {
            // Lifecycle stops the container tree before acknowledging stopped, including failures.
            if (proc_get_status($process)['running']) {
                proc_terminate($process, 9);
            }
            foreach ($pipes as $pipe) {
                fclose($pipe);
            }
            proc_close($process);
        }
    }

    public function stop(string $sandbox): void
    {
        if (preg_match('/^shipmunk-profile-[0-9a-hjkmnp-tv-z]{26}$/D', $sandbox) !== 1) {
            throw new RuntimeException('Invalid native profile sandbox.');
        }
        $inspect = $this->commands->run(['docker', 'inspect', $sandbox]);
        if ($inspect->exitCode === 0) {
            $this->commands->run(['docker', 'stop', '--time', '2', $sandbox]);
            $this->commands->run(['docker', 'rm', '--force', $sandbox]);
            $inspect = $this->commands->run(['docker', 'inspect', $sandbox]);
        }
        if ($inspect->exitCode === 0 || preg_match('/no such (?:object|container)/i', $inspect->stdout.$inspect->stderr) !== 1) {
            throw new RuntimeException('Cannot confirm native profile process tree absent.');
        }
    }
}
