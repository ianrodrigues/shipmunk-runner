<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use RuntimeException;

final readonly class WorkspacePreparer
{
    public function __construct(
        private string $root,
        private SafeTarExtractor $extractor = new SafeTarExtractor,
    ) {}

    public function path(Claim $claim): string
    {
        return $this->root.'/'.$claim->attemptId.'-'.$claim->fence;
    }

    public function ensureWritable(): void
    {
        if (! is_dir($this->root) && ! mkdir($this->root, 0700, true) && ! is_dir($this->root)) {
            throw new RuntimeException('Unable to create workspace root.');
        }

        $probe = $this->root.'/.write-probe-'.bin2hex(random_bytes(6));

        if (file_put_contents($probe, 'probe', LOCK_EX) === false || ! unlink($probe)) {
            throw new RuntimeException('Workspace root is not writable; refusing to claim work.');
        }
    }

    /**
     * @param  (\Closure(int): void)|null  $checkpoint
     */
    public function prepare(
        Claim $claim,
        ControlPlaneClient $client,
        ?\Closure $checkpoint = null,
    ): string {
        $workspace = $this->path($claim);

        if (file_exists($workspace)) {
            throw new RuntimeException('Attempt workspace already exists.');
        }

        if (! mkdir($workspace, 0700, true)) {
            throw new RuntimeException('Unable to create attempt workspace.');
        }

        try {
            $this->extractSources($claim, $client, $workspace.'/sources', $checkpoint);
            $this->storeInstructions($claim, $client, $workspace.'/instructions', $checkpoint);
        } catch (\Throwable $exception) {
            $this->remove($workspace);

            throw $exception;
        }

        return $workspace;
    }

    private function extractSources(
        Claim $claim,
        ControlPlaneClient $client,
        string $directory,
        ?\Closure $checkpoint,
    ): void {
        $references = $claim->manifest['source_artifacts'] ?? [];

        if (! is_array($references) || $references === []) {
            throw new RuntimeException('Claim source_artifacts is invalid.');
        }

        foreach (array_values($references) as $index => $reference) {
            if (! is_array($reference) || ! is_string($reference['artifact_id'] ?? null) || ! is_string($reference['sha256'] ?? null)) {
                throw new RuntimeException('Claim artifact reference is invalid.');
            }

            $target = $directory.'/'.(string) $index;

            if (! mkdir($target, 0700, true)) {
                throw new RuntimeException('Unable to create artifact workspace.');
            }

            $checkpoint?->__invoke(Protocol::HTTP_TIMEOUT_SECONDS);
            $archive = $client->downloadArtifact($claim, $reference['artifact_id'], $reference['sha256']);
            $checkpoint?->__invoke(0);
            $this->extractor->extract($archive, $target, $checkpoint);
            $revision = null;

            if (count($references) === 2 && $index === 0) {
                $revision = $claim->manifest['diff_base_sha'] ?? null;
            } elseif ($index === count($references) - 1) {
                $revision = $claim->manifest['head_sha'] ?? null;
            }

            $this->unwrapGitHubSnapshot($target, $revision, $checkpoint);
            $checkpoint?->__invoke(0);
        }
    }

    private function unwrapGitHubSnapshot(
        string $directory,
        mixed $revision,
        ?\Closure $checkpoint,
    ): void {
        if (! is_string($revision) || preg_match('/\A[0-9a-f]{40}\z/i', $revision) !== 1) {
            return;
        }

        $entries = scandir($directory);

        if ($entries === false) {
            throw new RuntimeException('Unable to inspect the source snapshot.');
        }

        $entries = array_values(array_diff($entries, ['.', '..']));

        if (count($entries) !== 1) {
            return;
        }

        $wrapper = $entries[0];
        $expectedSuffix = '(?:'.substr($revision, 0, 7).'|'.$revision.')';
        $source = $directory.'/'.$wrapper;

        if (preg_match('/\A.+-'.$expectedSuffix.'\z/i', $wrapper) !== 1
            || ! is_dir($source) || is_link($source)) {
            return;
        }

        $staging = $directory.'.archive-root';

        if (file_exists($staging) || is_link($staging)) {
            throw new RuntimeException('Archive normalization destination already exists.');
        }

        $checkpoint?->__invoke(0);

        if (! rename($source, $staging)) {
            throw new RuntimeException('Unable to stage the source archive root.');
        }

        $children = scandir($staging);

        if ($children === false) {
            throw new RuntimeException('Unable to inspect the source archive root.');
        }

        $nextCheckpoint = microtime(true) + 5;

        foreach (array_diff($children, ['.', '..']) as $child) {
            $destination = $directory.'/'.$child;

            if (file_exists($destination) || is_link($destination)
                || ! rename($staging.'/'.$child, $destination)) {
                throw new RuntimeException('Unable to normalize the source archive root.');
            }

            if ($checkpoint !== null && microtime(true) >= $nextCheckpoint) {
                $checkpoint(0);
                $nextCheckpoint = microtime(true) + 5;
            }
        }

        if (! rmdir($staging)) {
            throw new RuntimeException('Unable to remove the source archive wrapper.');
        }

        $checkpoint?->__invoke(0);
    }

    private function storeInstructions(
        Claim $claim,
        ControlPlaneClient $client,
        string $directory,
        ?\Closure $checkpoint,
    ): void {
        $references = $claim->manifest['instruction_artifacts'] ?? [];

        if (! is_array($references) || $references === [] || ! mkdir($directory, 0700, true)) {
            throw new RuntimeException('Claim instruction_artifacts is invalid.');
        }

        foreach (array_values($references) as $index => $reference) {
            if (! is_array($reference) || ! is_string($reference['artifact_id'] ?? null) || ! is_string($reference['sha256'] ?? null)) {
                throw new RuntimeException('Claim instruction artifact reference is invalid.');
            }

            $checkpoint?->__invoke(Protocol::HTTP_TIMEOUT_SECONDS);
            $bytes = $client->downloadArtifact($claim, $reference['artifact_id'], $reference['sha256']);
            $checkpoint?->__invoke(0);
            $checkpoint?->__invoke(Protocol::HTTP_TIMEOUT_SECONDS);

            try {
                $instructions = json_decode($bytes, true, flags: JSON_THROW_ON_ERROR);
            } catch (\JsonException $exception) {
                throw new RuntimeException('Trusted instruction artifact is not valid JSON.', previous: $exception);
            }

            if (! is_array($instructions)) {
                throw new RuntimeException('Trusted instruction artifact must be a JSON object.');
            }

            $path = $directory.'/'.(string) $index.'.json';
            $checkpoint?->__invoke(Protocol::HTTP_TIMEOUT_SECONDS);

            if (file_put_contents($path, $bytes, LOCK_EX) !== strlen($bytes) || ! chmod($path, 0600)) {
                throw new RuntimeException('Unable to store trusted instruction artifact.');
            }

            $checkpoint?->__invoke(0);
        }
    }

    public function remove(string $workspace): void
    {
        if (! str_starts_with($workspace, rtrim($this->root, '/').'/') || ! is_dir($workspace)) {
            return;
        }

        $iterator = new \RecursiveIteratorIterator(
            new \RecursiveDirectoryIterator($workspace, \FilesystemIterator::SKIP_DOTS),
            \RecursiveIteratorIterator::CHILD_FIRST,
        );

        foreach ($iterator as $entry) {
            if (! $entry instanceof \SplFileInfo) {
                throw new RuntimeException('Attempt workspace contains an invalid entry.');
            }

            $removed = $entry->isDir() && ! $entry->isLink()
                ? @rmdir($entry->getPathname())
                : @unlink($entry->getPathname());

            if (! $removed) {
                throw new RuntimeException('Unable to remove attempt workspace contents.');
            }
        }

        if (! @rmdir($workspace)) {
            throw new RuntimeException('Unable to remove attempt workspace.');
        }
    }
}
