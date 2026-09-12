<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use RuntimeException;

/**
 * Atomic replacement and file fsync protect recovery from process crashes.
 * Directory entries are not fsynced; abrupt host power loss can lose the latest
 * rename or deletion. Recovery therefore also depends on server lease fencing.
 */
final readonly class AttemptStateStore
{
    public function __construct(
        private string $path,
    ) {}

    public function ensureWritable(): void
    {
        $directory = $this->directory();
        $probe = $directory.'/.write-probe-'.bin2hex(random_bytes(6));

        if (file_put_contents($probe, 'probe', LOCK_EX) === false
            || ! unlink($probe)) {
            throw new RuntimeException('Runner state directory is not writable; refusing to claim work.');
        }
    }

    /**
     * @return array{run_id: string, attempt_id: string, fence: int, profile_id: string|null, sandbox_id: string|null, lease_expires_at: string, deadline: string, workspace: string}|null
     */
    public function load(): ?array
    {
        if (! is_file($this->path)) {
            return null;
        }

        $json = file_get_contents($this->path);
        $data = $json === false ? null : json_decode($json, true);

        if (! is_array($data)
            || ! is_string($data['run_id'] ?? null)
            || ! is_string($data['attempt_id'] ?? null)
            || ! is_int($data['fence'] ?? null)
            || ! is_string($data['lease_expires_at'] ?? null)
            || ! is_string($data['deadline'] ?? null)
            || ! is_string($data['workspace'] ?? null)) {
            throw new RuntimeException('Runner state file is invalid; refusing to claim replacement work.');
        }

        $profileId = $data['profile_id'] ?? null;

        if (! is_string($profileId)
            && $profileId !== null) {
            throw new RuntimeException('Runner profile state is invalid.');
        }

        $sandboxId = $data['sandbox_id'] ?? null;

        if (! is_string($sandboxId)
            && $sandboxId !== null) {
            throw new RuntimeException('Runner sandbox state is invalid.');
        }

        return [
            'run_id' => $data['run_id'],
            'attempt_id' => $data['attempt_id'],
            'fence' => $data['fence'],
            'profile_id' => $profileId,
            'sandbox_id' => $sandboxId,
            'lease_expires_at' => $data['lease_expires_at'],
            'deadline' => $data['deadline'],
            'workspace' => $data['workspace'],
        ];
    }

    public function save(
        Claim $claim,
        ?SandboxProcess $process,
        \DateTimeImmutable $leaseExpiresAt,
        string $workspace,
    ): void {
        $this->directory();
        $temporary = $this->path.'.tmp-'.bin2hex(random_bytes(6));
        $json = json_encode([
            'run_id' => $claim->runId,
            'attempt_id' => $claim->attemptId,
            'fence' => $claim->fence,
            'profile_id' => $claim->manifest['profile_id'] ?? null,
            'sandbox_id' => $process?->id(),
            'lease_expires_at' => $leaseExpiresAt->format(DATE_ATOM),
            'deadline' => $claim->deadline->format(DATE_ATOM),
            'workspace' => $workspace,
        ], JSON_THROW_ON_ERROR | JSON_UNESCAPED_SLASHES);

        $this->writeDurably($temporary, $json);

        if (! rename($temporary, $this->path)) {
            @unlink($temporary);

            throw new RuntimeException('Unable to commit runner state.');
        }
    }

    public function clear(): void
    {
        if (is_file($this->path)
            && ! unlink($this->path)) {
            throw new RuntimeException('Unable to clear runner state.');
        }
    }

    private function directory(): string
    {
        $directory = dirname($this->path);

        if (! is_dir($directory)
            && ! mkdir($directory, 0700, true)
            && ! is_dir($directory)) {
            throw new RuntimeException('Unable to create runner state directory.');
        }

        return $directory;
    }

    private function writeDurably(
        string $path,
        string $contents,
    ): void {
        $stream = @fopen($path, 'xb');

        if ($stream === false) {
            throw new RuntimeException('Unable to create runner state.');
        }

        try {
            if (! flock($stream, LOCK_EX)
                || fwrite($stream, $contents) !== strlen($contents)
                || ! fflush($stream)
                || (function_exists('fsync')
                    && ! fsync($stream))
                || ! chmod($path, 0600)) {
                throw new RuntimeException('Unable to write runner state durably.');
            }
        } catch (\Throwable $exception) {
            @unlink($path);

            throw $exception;
        } finally {
            fclose($stream);
        }
    }
}
