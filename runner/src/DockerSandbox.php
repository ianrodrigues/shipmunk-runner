<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use InvalidArgumentException;
use RuntimeException;

final readonly class DockerSandbox implements Sandbox
{
    public function __construct(
        private string $image,
        private CommandRunner $commands = new CommandRunner,
        private int $memoryMegabytes = 256,
        private int $workspaceMegabytes = 128,
        private int $pids = 64,
        private string $cpus = '1.0',
    ) {
        if ($memoryMegabytes < 32 || $workspaceMegabytes < 8 || $pids < 8) {
            throw new InvalidArgumentException('Sandbox resource limits are too small.');
        }
    }

    public function create(Claim $claim, array $agentInput, string $workspace): SandboxProcess
    {
        if (! is_dir($workspace)) {
            throw new InvalidArgumentException('Sandbox workspace does not exist.');
        }

        $name = 'shipmunk-'.$claim->attemptId.'-'.$claim->fence;
        $command = [
            'docker', 'create',
            '--name', $name,
            '--label', 'shipmunk.runner=true',
            '--label', 'shipmunk.attempt='.$claim->attemptId,
            '--read-only',
            '--user', '65532:65532',
            '--cap-drop', 'ALL',
            '--security-opt', 'no-new-privileges:true',
            '--network', 'none',
            '--memory', $this->memoryMegabytes.'m',
            '--memory-swap', $this->memoryMegabytes.'m',
            '--cpus', $this->cpus,
            '--pids-limit', (string) $this->pids,
            '--ulimit', 'nofile=256:256',
            '--stop-timeout', '2',
            '--log-driver', 'json-file',
            '--log-opt', 'compress=false',
            '--log-opt', 'max-size=1m',
            '--log-opt', 'max-file=1',
            '--tmpfs', "/workspace:rw,exec,nosuid,nodev,size={$this->workspaceMegabytes}m,uid=65532,gid=65532,mode=700",
            '--tmpfs', '/run/shipmunk:rw,noexec,nosuid,nodev,size=1m,uid=65532,gid=65532,mode=700',
            '--tmpfs', '/tmp:rw,noexec,nosuid,nodev,size=16m,uid=65532,gid=65532,mode=700',
            '--env', 'HOME=/workspace',
            '--env', 'PATH=/usr/local/bin:/usr/bin:/bin',
            '--workdir', '/workspace',
            $this->image,
            $claim->runId,
            $claim->attemptId,
            (string) $claim->fence,
        ];

        $containerId = trim($this->commands->mustRun($command)->stdout);

        if ($containerId === '') {
            throw new RuntimeException('Docker did not return a sandbox identifier.');
        }

        return new DockerSandboxProcess($containerId, $this->commands, $workspace, $agentInput);
    }

    public function reconcile(string $sandboxId): void
    {
        if (preg_match('/^[a-f0-9]{12,64}$/', $sandboxId) !== 1) {
            throw new InvalidArgumentException('Unsafe sandbox identifier.');
        }

        $inspect = $this->commands->run(['docker', 'inspect', $sandboxId]);

        if ($inspect->exitCode !== 0) {
            if ($this->sandboxIsAbsent($inspect)) {
                return;
            }

            throw new RuntimeException('Unable to confirm sandbox state during reconciliation.');
        }

        $stop = $this->commands->run(['docker', 'stop', '--time', '2', $sandboxId]);

        if ($stop->exitCode !== 0) {
            $this->commands->mustRun(['docker', 'kill', $sandboxId]);
        }

        $this->commands->mustRun(['docker', 'rm', '--force', $sandboxId]);

        $confirmation = $this->commands->run(['docker', 'inspect', $sandboxId]);

        if ($confirmation->exitCode === 0 || ! $this->sandboxIsAbsent($confirmation)) {
            throw new RuntimeException('Unable to confirm sandbox removal.');
        }
    }

    private function sandboxIsAbsent(CommandResult $result): bool
    {
        return preg_match('/no such (?:object|container)/i', $result->stdout.$result->stderr) === 1;
    }
}
