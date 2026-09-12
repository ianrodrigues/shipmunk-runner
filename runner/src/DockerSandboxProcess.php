<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use RuntimeException;

final readonly class DockerSandboxProcess implements SandboxProcess
{
    /** @param array<string, mixed> $agentInput */
    public function __construct(
        private string $containerId,
        private CommandRunner $commands,
        private string $workspace,
        private array $agentInput,
    ) {}

    public function id(): string
    {
        return $this->containerId;
    }

    public function start(?\Closure $checkpoint = null): void
    {
        try {
            $this->commands->mustRun(['docker', 'start', $this->containerId]);
            $checkpoint?->__invoke(0);
            $this->copyWorkspace($checkpoint);
            $checkpoint?->__invoke(0);
            $this->copyAgentInput();
            $checkpoint?->__invoke(0);
            $this->commands->mustRun([
                'docker', 'exec', $this->containerId,
                'sh', '-c', 'touch /run/shipmunk/ready.tmp && mv /run/shipmunk/ready.tmp /run/shipmunk/ready',
            ]);
            $checkpoint?->__invoke(0);
        } catch (\Throwable $exception) {
            try {
                $this->stop();
            } catch (\Throwable) {
                throw new RuntimeException(
                    'Sandbox startup failed, and its cleanup also failed.',
                    previous: $exception,
                );
            }

            throw $exception;
        }
    }

    /** @phpstan-impure */
    public function isRunning(): bool
    {
        return trim($this->commands->mustRun([
            'docker', 'inspect', '--format', '{{.State.Running}}', $this->containerId,
        ])->stdout) === 'true';
    }

    public function exitCode(): ?int
    {
        if ($this->isRunning()) {
            return null;
        }

        return (int) trim($this->commands->mustRun([
            'docker', 'inspect', '--format', '{{.State.ExitCode}}', $this->containerId,
        ])->stdout);
    }

    public function output(): string
    {
        $output = $this->commands->mustRun(['docker', 'logs', $this->containerId])->stdout;

        if (strlen($output) > 2 * 1024 * 1024) {
            throw new RuntimeException('Native output exceeded 2 MiB.');
        }

        return $output;
    }

    public function stop(): void
    {
        if (! $this->isRunning()) {
            return;
        }

        $stop = $this->commands->run(['docker', 'stop', '--time', '2', $this->containerId]);

        if ($stop->exitCode !== 0 || $this->isRunning()) {
            $this->commands->mustRun(['docker', 'kill', $this->containerId]);
        }

        if ($this->isRunning()) {
            throw new RuntimeException('Sandbox process tree survived TERM and KILL.');
        }
    }

    public function remove(): void
    {
        $this->stop();
        $this->commands->mustRun(['docker', 'rm', '--force', $this->containerId]);
    }

    private function copyWorkspace(?\Closure $checkpoint): void
    {
        $archive = $this->commands->mustRun([
            'tar',
            '--create',
            '--file',
            '-',
            '--directory',
            $this->workspace,
            '.',
        ], timeoutSeconds: 30)->stdout;

        $checkpoint?->__invoke(0);
        $this->commands->mustRun(
            ['docker', 'exec', '--interactive', $this->containerId, 'tar', '-xf', '-', '-C', '/workspace'],
            $archive,
            timeoutSeconds: 30,
        );
    }

    private function copyAgentInput(): void
    {
        $json = json_encode($this->agentInput, JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES);

        $this->commands->mustRun([
            'docker', 'exec', '--interactive', $this->containerId,
            'sh', '-c', 'umask 077; cat > /run/shipmunk/agent-input.json.tmp && mv /run/shipmunk/agent-input.json.tmp /run/shipmunk/agent-input.json',
        ], $json);
    }
}
