<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use RuntimeException;

final readonly class SafeTarExtractor
{
    public function __construct(
        private int $maxFiles = 20_000,
        private int $maxBytes = 128 * 1024 * 1024,
    ) {}

    /**
     * @param  (\Closure(int): void)|null  $checkpoint
     */
    public function extract(
        string $archive,
        string $destination,
        ?\Closure $checkpoint = null,
    ): void {
        $archive = $this->decompress($archive, $checkpoint);

        if (! is_dir($destination) || is_link($destination)) {
            throw new RuntimeException('Extraction destination must be a real directory.');
        }

        $offset = 0;
        $files = 0;
        $bytes = 0;
        $length = strlen($archive);
        $nextCheckpoint = microtime(true) + 5;
        $pendingPath = null;

        while ($offset + 512 <= $length) {
            if ($checkpoint !== null && microtime(true) >= $nextCheckpoint) {
                $checkpoint(0);
                $nextCheckpoint = microtime(true) + 5;
            }

            $header = substr($archive, $offset, 512);
            $offset += 512;

            if ($header === str_repeat("\0", 512)) {
                if ($pendingPath !== null) {
                    throw new RuntimeException('Archive contains an unused path extension.');
                }

                return;
            }

            $this->verifyChecksum($header);
            $size = $this->octal(substr($header, 124, 12));
            $type = $header[156];
            $files++;
            $bytes += $size;

            if ($files > $this->maxFiles || $bytes > $this->maxBytes) {
                throw new RuntimeException('Archive exceeded extraction limits.');
            }

            if ($offset + $size > $length) {
                throw new RuntimeException('Archive entry is truncated.');
            }

            if ($type === 'g' || $type === 'x') {
                if ($pendingPath !== null || $size > 65_536) {
                    throw new RuntimeException('Archive path extension is invalid.');
                }

                $this->path($header, $type);
                $attributes = $this->paxAttributes(substr($archive, $offset, $size), $type);
                $pendingPath = $attributes['path'] ?? null;
                $offset += (int) (ceil($size / 512) * 512);

                continue;
            }

            $name = $this->path($header, $type, $pendingPath);
            $pendingPath = null;
            $target = $destination.'/'.$name;

            if ($type === '5') {
                $this->makeDirectory($target);
            } elseif ($type === "\0" || $type === '0') {
                $this->makeDirectory(dirname($target));

                if (is_link($target)) {
                    throw new RuntimeException('Archive target is a symbolic link.');
                }

                if (file_put_contents($target, substr($archive, $offset, $size), LOCK_EX) !== $size) {
                    throw new RuntimeException('Unable to extract archive entry.');
                }

                $mode = $this->octal(substr($header, 100, 8));

                // Git records executable intent from the owner bit, not group or other permissions.
                if (! chmod($target, ($mode & 0100) !== 0 ? 0700 : 0600)) {
                    throw new RuntimeException('Unable to set archive file permissions.');
                }
            } else {
                throw new RuntimeException('Archive links and special files are forbidden.');
            }

            $offset += (int) (ceil($size / 512) * 512);
        }

        throw new RuntimeException('Archive is missing its end marker.');
    }

    private function path(
        string $header,
        string $type,
        ?string $override = null,
    ): string {
        $name = rtrim(substr($header, 0, 100), "\0");
        $prefix = rtrim(substr($header, 345, 155), "\0");
        $path = $override ?? ($prefix === '' ? $name : $prefix.'/'.$name);

        if ($type === '5' && str_ends_with($path, '/')) {
            $path = substr($path, 0, -1);
        }

        if ($path === '' || str_starts_with($path, '/') || str_contains($path, '\\') || str_contains($path, "\0")) {
            throw new RuntimeException('Archive path is unsafe.');
        }

        foreach (explode('/', $path) as $segment) {
            if ($segment === '' || $segment === '.' || $segment === '..') {
                throw new RuntimeException('Archive path traversal is forbidden.');
            }
        }

        return $path;
    }

    /**
     * @return array<string, string>
     */
    private function paxAttributes(
        string $payload,
        string $type,
    ): array {
        $attributes = [];
        $offset = 0;
        $size = strlen($payload);
        $allowed = $type === 'g' ? 'comment' : 'path';

        while ($offset < $size) {
            if (preg_match('/\A([1-9][0-9]{0,5}) /', substr($payload, $offset), $matches) !== 1) {
                throw new RuntimeException('Archive extension record length is invalid.');
            }

            $length = (int) $matches[1];
            $prefixLength = strlen($matches[0]);

            if ($length <= $prefixLength + 1 || $length > $size - $offset
                || $payload[$offset + $length - 1] !== "\n") {
                throw new RuntimeException('Archive extension record is truncated.');
            }

            $record = substr($payload, $offset + $prefixLength, $length - $prefixLength - 1);
            $separator = strpos($record, '=');

            if ($separator === false) {
                throw new RuntimeException('Archive extension record is invalid.');
            }

            $key = substr($record, 0, $separator);

            if ($key !== $allowed || array_key_exists($key, $attributes)) {
                throw new RuntimeException('Archive extension attribute is unsupported.');
            }

            $attributes[$key] = substr($record, $separator + 1);
            $offset += $length;
        }

        if ($type === 'x' && ! isset($attributes['path'])) {
            throw new RuntimeException('Archive path extension is missing its path.');
        }

        return $attributes;
    }

    private function makeDirectory(string $directory): void
    {
        if (is_link($directory)) {
            throw new RuntimeException('Archive directory is a symbolic link.');
        }

        if (! is_dir($directory) && ! mkdir($directory, 0700, true) && ! is_dir($directory)) {
            throw new RuntimeException('Unable to create archive directory.');
        }

        chmod($directory, 0700);
    }

    private function verifyChecksum(string $header): void
    {
        $expected = $this->octal(substr($header, 148, 8));
        $check = substr_replace($header, str_repeat(' ', 8), 148, 8);
        $bytes = unpack('C*', $check);

        if ($bytes === false || array_sum($bytes) !== $expected) {
            throw new RuntimeException('Archive header checksum is invalid.');
        }
    }

    private function octal(string $value): int
    {
        $value = trim($value, " \0");

        if ($value === '' || preg_match('/^[0-7]+$/', $value) !== 1) {
            return $value === '' ? 0 : throw new RuntimeException('Archive size is invalid.');
        }

        return intval($value, 8);
    }

    private function decompress(
        string $archive,
        ?\Closure $checkpoint,
    ): string {
        if (! str_starts_with($archive, "\x1f\x8b")) {
            return $archive;
        }

        $inflate = inflate_init(ZLIB_ENCODING_GZIP);

        if ($inflate === false) {
            throw new RuntimeException('Unable to initialize gzip extraction.');
        }

        $decoded = '';
        $length = strlen($archive);
        $nextCheckpoint = microtime(true) + 5;

        for ($offset = 0; $offset < $length; $offset += 4096) {
            $flush = $offset + 4096 >= $length ? ZLIB_FINISH : ZLIB_SYNC_FLUSH;
            $chunk = inflate_add($inflate, substr($archive, $offset, 4096), $flush);

            if ($chunk === false || strlen($decoded) + strlen($chunk) > $this->maxBytes) {
                throw new RuntimeException('Compressed archive exceeded extraction limits.');
            }

            $decoded .= $chunk;

            if ($checkpoint !== null && microtime(true) >= $nextCheckpoint) {
                $checkpoint(0);
                $nextCheckpoint = microtime(true) + 5;
            }
        }

        return $decoded;
    }
}
