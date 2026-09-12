<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use RuntimeException;

final class CommandRunner
{
    /** @param list<string> $command */
    public function run(array $command, string $stdin = '', int $timeoutSeconds = 10): CommandResult
    {
        $pipes = [];
        $process = proc_open($command, [
            0 => ['pipe', 'r'],
            1 => ['pipe', 'w'],
            2 => ['pipe', 'w'],
        ], $pipes);

        if (! is_resource($process)) {
            throw new RuntimeException('Unable to start subprocess.');
        }

        foreach ($pipes as $pipe) {
            stream_set_blocking($pipe, false);
        }

        return $this->communicate($process, $pipes, $stdin, $timeoutSeconds);
    }

    /** @param list<string> $command */
    public function mustRun(array $command, string $stdin = '', int $timeoutSeconds = 10): CommandResult
    {
        $result = $this->run($command, $stdin, $timeoutSeconds);

        if ($result->exitCode !== 0) {
            throw new RuntimeException('Subprocess failed: '.trim($result->stderr));
        }

        return $result;
    }

    /**
     * @param  resource  $process
     * @param  array<int, resource>  $pipes
     */
    private function communicate(mixed $process, array $pipes, string $stdin, int $timeoutSeconds): CommandResult
    {
        $stdout = '';
        $stderr = '';
        $written = 0;
        $started = microtime(true);
        $status = proc_get_status($process);

        while ($status['running']) {
            if (microtime(true) - $started >= $timeoutSeconds) {
                $this->terminate($process, $pipes);

                throw new RuntimeException('Subprocess exceeded its time limit.');
            }

            $progress = $this->writeInput($pipes[0], $stdin, $written);
            $progress += $this->readOutput($pipes, $stdout, $stderr);

            if (strlen($stdout) + strlen($stderr) > 256 * 1024 * 1024) {
                $this->terminate($process, $pipes, graceMilliseconds: 0);

                throw new RuntimeException('Subprocess output exceeded its byte limit.');
            }

            if ($progress === 0) {
                usleep(10_000);
            }
            $status = proc_get_status($process);
        }

        $stdout .= (string) stream_get_contents($pipes[1]);
        $stderr .= (string) stream_get_contents($pipes[2]);
        $this->closePipes($pipes);
        $closed = proc_close($process);
        $exitCode = $status['exitcode'] >= 0 ? $status['exitcode'] : $closed;

        return new CommandResult($exitCode, $stdout, $stderr);
    }

    /** @param resource $pipe */
    private function writeInput(mixed $pipe, string $stdin, int &$written): int
    {
        if (! is_resource($pipe)) {
            return 0;
        }

        if ($written >= strlen($stdin)) {
            fclose($pipe);

            return 1;
        }

        $chunk = fwrite($pipe, substr($stdin, $written, 65_536));
        $written += $chunk === false ? 0 : $chunk;

        return $chunk === false ? 0 : $chunk;
    }

    /** @param array<int, resource> $pipes */
    private function readOutput(array $pipes, string &$stdout, string &$stderr): int
    {
        $stdoutChunk = (string) fread($pipes[1], 65_536);
        $stderrChunk = (string) fread($pipes[2], 65_536);
        $stdout .= $stdoutChunk;
        $stderr .= $stderrChunk;

        return strlen($stdoutChunk) + strlen($stderrChunk);
    }

    /**
     * @param  resource  $process
     * @param  array<int, resource>  $pipes
     */
    private function terminate(mixed $process, array $pipes, int $graceMilliseconds = 200): void
    {
        proc_terminate($process, $graceMilliseconds === 0 ? 9 : 15);

        if ($graceMilliseconds > 0) {
            usleep($graceMilliseconds * 1000);

            if (proc_get_status($process)['running']) {
                proc_terminate($process, 9);
            }
        }

        $this->closePipes($pipes);
        proc_close($process);
    }

    /** @param array<int, resource> $pipes */
    private function closePipes(array $pipes): void
    {
        foreach ($pipes as $pipe) {
            if (is_resource($pipe)) {
                fclose($pipe);
            }
        }
    }
}
