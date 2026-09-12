<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Profiles;

use Closure;
use FilesystemIterator;
use RuntimeException;
use Shipmunk\Runner\Claim;

/**
 * Trusted runner-host storage. Never expose this root to native or repository processes.
 */
final class ProfileStore
{
    private string $directory;

    /**
     * @var resource|null
     */
    private mixed $lock = null;

    public function __construct(
        string $root,
        public readonly string $profileId,
    ) {
        self::identifier($profileId);

        if (! str_starts_with($root, '/')
            || str_contains($root, '..')
            || str_contains($root, ',')) {
            throw new RuntimeException('Profile root must be an absolute, canonical path.');
        }

        $path = '';
        foreach (explode('/', trim($root, '/')) as $part) {
            $path .= '/'.$part;
            if (is_link($path)) {
                throw new RuntimeException('Symlinks are forbidden in the profile root.');
            }
        }

        self::directory($root);
        $this->directory = rtrim($root, '/').'/'.$profileId;
        self::directory($this->directory);
    }

    public static function identifier(string $id): void
    {
        if (preg_match('/^[0-9a-hjkmnp-tv-z]{26}$/D', $id) !== 1) {
            throw new RuntimeException('Invalid profile or operation identifier.');
        }
    }

    private static function directory(string $path): void
    {
        if (! file_exists($path)
            && ! is_link($path)
            && ! mkdir($path, 0700)) {
            throw new RuntimeException('Cannot create protected profile directory.');
        }

        self::protect($path, true);
    }

    public static function protect(
        string $path,
        bool $directory = false,
    ): void {
        clearstatcache(true, $path);
        $stat = lstat($path);
        $type = $directory ? 0040000 : 0100000;
        $permissions = $directory ? 0700 : 0600;

        if ($stat === false
            || ($stat['mode'] & 0170000) !== $type
            || ($stat['mode'] & 0777) !== $permissions
            || $stat['uid'] !== posix_geteuid()
            || (! $directory
                && $stat['nlink'] !== 1)) {
            throw new RuntimeException('Unsafe profile store ownership, type or permissions.');
        }
    }

    /**
     * Login, refresh, disconnect and future trusted native execution must use this same lock.
     */
    public function exclusively(Closure $operation): mixed
    {
        $path = $this->directory.'/lock';
        if (! file_exists($path)
            && ! is_link($path)) {
            $old = umask(0077);
            $created = @fopen($path, 'x');
            umask($old);
            if (is_resource($created)) {
                fclose($created);
            }
        }
        self::protect($path);
        $lock = fopen($path, 'r+');
        if ($lock === false
            || ! flock($lock, LOCK_EX | LOCK_NB)) {
            if (is_resource($lock)) {
                fclose($lock);
            }
            throw new RuntimeException('Profile is already in use.');
        }

        try {
            if (fstat($lock)['ino'] !== lstat($path)['ino']) {
                throw new RuntimeException('Profile lock was replaced.');
            }

            $this->lock = $lock;

            return $operation($this);
        } finally {
            flock($lock, LOCK_UN);
            fclose($lock);
            $this->lock = null;
        }
    }

    public function home(): string
    {
        return $this->directory.'/home';
    }

    public function createHome(): string
    {
        self::directory($this->home());

        return $this->home();
    }

    /**
     * @return array<string, mixed>|null
     */
    public function read(string $name): ?array
    {
        $path = $this->path($name);
        if (! file_exists($path)
            && ! is_link($path)) {
            return null;
        }
        self::protect($path);
        $bytes = file_get_contents($path, length: 16_385);
        if ($bytes === false
            || strlen($bytes) > 16_384) {
            throw new RuntimeException('Invalid profile journal.');
        }
        $data = json_decode($bytes, true, flags: JSON_THROW_ON_ERROR);
        if (! is_array($data)) {
            throw new RuntimeException('Invalid profile journal.');
        }

        return $data;
    }

    public function write(
        string $name,
        array $data,
    ): void {
        $bytes = json_encode($data, JSON_THROW_ON_ERROR);
        $path = $this->path($name);
        $temporary = $path.'.'.bin2hex(random_bytes(8));
        $old = umask(0077);
        $file = fopen($temporary, 'x');
        umask($old);
        if ($file === false) {
            throw new RuntimeException('Cannot write profile journal.');
        }
        try {
            if (fwrite($file, $bytes) !== strlen($bytes)
                || ! fflush($file)
                || ! fsync($file)) {
                throw new RuntimeException('Cannot persist profile journal.');
            }
            if (! rename($temporary, $path)) {
                throw new RuntimeException('Cannot activate profile journal.');
            }
        } finally {
            fclose($file);
            if (file_exists($temporary)
                || is_link($temporary)) {
                unlink($temporary);
            }
        }
    }

    public function forget(string $name): void
    {
        $path = $this->path($name);
        if ((file_exists($path)
            || is_link($path))
            && ! unlink($path)) {
            throw new RuntimeException('Cannot invalidate profile journal.');
        }
    }

    private function path(string $name): string
    {
        if (! in_array($name, ['active', 'pending', 'completed', 'execution'], true)) {
            throw new RuntimeException('Invalid profile journal name.');
        }

        return $this->directory.'/'.$name.'.json';
    }

    public function validateHome(): void
    {
        $this->validateTree($this->home());
    }

    public function reserveExecution(Claim $claim): void
    {
        $this->assertLocked();

        if ($this->read('execution') !== null) {
            throw new RuntimeException('Profile execution requires stopped recovery.');
        }

        $this->write('execution', $this->executionIdentity($claim));
    }

    public function assertExecutionMatches(Claim $claim): bool
    {
        $this->assertLocked();
        $execution = $this->read('execution');

        if ($execution === null) {
            return false;
        }

        if ($execution !== $this->executionIdentity($claim)) {
            throw new RuntimeException('Profile execution belongs to another attempt.');
        }

        return true;
    }

    /**
     * Call only after every process tree for this reservation is confirmed absent.
     */
    public function releaseExecution(Claim $claim): void
    {
        if (! $this->assertExecutionMatches($claim)) {
            return;
        }

        $this->normalizeNativeHome();
        $this->forget('execution');
    }

    /**
     * @return array{profile_id: string, run_id: string, attempt_id: string, fence: int, sandbox_id: string}
     */
    private function executionIdentity(Claim $claim): array
    {
        if (($claim->manifest['profile_id'] ?? null) !== $this->profileId) {
            throw new RuntimeException('Profile execution identity mismatch.');
        }

        return [
            'profile_id' => $this->profileId,
            'run_id' => $claim->runId,
            'attempt_id' => $claim->attemptId,
            'fence' => $claim->fence,
            'sandbox_id' => 'shipmunk-codex-'.$claim->attemptId.'-'.$claim->fence,
        ];
    }

    private function assertLocked(): void
    {
        if (! is_resource($this->lock)) {
            throw new RuntimeException('Profile execution journal requires the profile lock.');
        }
    }

    /**
     * Call only after the native process tree has stopped, while holding the profile lock.
     */
    public function normalizeNativeHome(): void
    {
        if (! is_resource($this->lock)) {
            throw new RuntimeException('Native home normalization requires the profile lock.');
        }

        self::protect(dirname($this->directory), true);
        self::protect($this->directory, true);
        self::protect($this->home(), true);

        // Validate every entry before changing any permissions. Native clients may explicitly
        // create 0644 files even under umask 0077; the enclosing home remains private.
        $entries = [];
        $this->inspectNativeTree($this->home(), $entries);

        foreach ($entries as $path => $expected) {
            clearstatcache(true, $path);
            $current = lstat($path);
            foreach (['dev', 'ino', 'mode', 'uid', 'nlink'] as $field) {
                if ($current === false
                    || $current[$field] !== $expected[$field]) {
                    throw new RuntimeException('Native home changed during normalization.');
                }
            }
            $directory = ($expected['mode'] & 0170000) === 0040000;
            if (! chmod($path, $directory ? 0700 : 0600)) {
                throw new RuntimeException('Cannot protect native-generated profile entry.');
            }
        }

        $this->validateHome();
    }

    private function inspectNativeTree(
        string $path,
        array &$entries,
    ): void {
        foreach (new FilesystemIterator($path) as $entry) {
            $child = $entry->getPathname();
            clearstatcache(true, $child);
            $stat = lstat($child);
            $type = $stat === false ? 0 : $stat['mode'] & 0170000;
            if ($stat === false
                || ! in_array($type, [0040000, 0100000], true)
                || $stat['uid'] !== posix_geteuid()
                || ($stat['mode'] & 07000) !== 0
                || ($type === 0100000
                    && $stat['nlink'] !== 1)) {
                throw new RuntimeException('Unsafe native-generated profile entry.');
            }

            $entries[$child] = $stat;
            if ($type === 0040000) {
                $this->inspectNativeTree($child, $entries);
            }
        }
    }

    private function validateTree(string $path): void
    {
        self::protect($path, true);
        foreach (new FilesystemIterator($path) as $entry) {
            if ($entry->isDir()
                && ! $entry->isLink()) {
                $this->validateTree($entry->getPathname());
            } else {
                self::protect($entry->getPathname());
            }
        }
    }

    public function invalidate(): void
    {
        $this->forget('active');
        $this->removeTree($this->home());
    }

    private function removeTree(string $path): void
    {
        if (is_link($path)
            || is_file($path)) {
            if (! unlink($path)) {
                throw new RuntimeException('Cannot remove profile store entry.');
            }
        } elseif (is_dir($path)) {
            foreach (new FilesystemIterator($path) as $entry) {
                $this->removeTree($entry->getPathname());
            }
            if (! rmdir($path)) {
                throw new RuntimeException('Cannot remove profile home.');
            }
        }
    }
}
