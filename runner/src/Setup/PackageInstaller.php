<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Setup;

use FilesystemIterator;
use RecursiveDirectoryIterator;
use RecursiveIteratorIterator;
use RuntimeException;

/**
 * Embedded in the public bootstrap so package validation runs before loading downloaded code.
 */
final class PackageInstaller
{
    public static function install(
        string $archive,
        string $home,
        string $digest,
        array $manifest,
    ): string {
        if (preg_match('/^[a-f0-9]{64}$/D', $digest) !== 1 || strlen($archive) > 2097152 || ! hash_equals($digest, hash('sha256', $archive))) {
            throw new RuntimeException('Package integrity check failed. Download the setup script again.');
        }
        $files = self::readArchive($archive, $manifest);
        self::home($home);
        $releases = $home.'/.shipmunk/releases';
        foreach ([$home.'/.shipmunk', $releases] as $directory) {
            if (! file_exists($directory) && ! is_link($directory) && ! mkdir($directory, 0700)) {
                throw new RuntimeException('Cannot create private runner installation.');
            }
            self::protect($directory, true);
        }
        $release = $releases.'/'.$digest;
        if (file_exists($release) || is_link($release)) {
            self::verify($release, $manifest);

            return $release;
        }
        $staging = $releases.'/.staging-'.bin2hex(random_bytes(12));
        if (! mkdir($staging, 0700)) {
            throw new RuntimeException('Cannot create private package staging.');
        }
        try {
            foreach ($files as $path => $contents) {
                $parent = dirname($staging.'/'.$path);
                if (! is_dir($parent) && ! mkdir($parent, 0700, recursive: true)) {
                    throw new RuntimeException('Cannot create package directories.');
                }
                $file = fopen($staging.'/'.$path, 'x');
                if ($file === false) {
                    throw new RuntimeException('Cannot install runner package file.');
                }
                try {
                    if (! chmod($staging.'/'.$path, 0600) || fwrite($file, $contents) !== strlen($contents) || ! fflush($file) || ! fsync($file)) {
                        throw new RuntimeException('Cannot persist runner package file.');
                    }
                } finally {
                    fclose($file);
                }
            }
            self::verify($staging, $manifest);
            if (! rename($staging, $release)) {
                throw new RuntimeException('Cannot activate runner package. Retry setup.');
            }
        } finally {
            self::remove($staging);
        }

        return $release;
    }

    private static function readArchive(
        string $archive,
        array $manifest,
    ): array {
        if ($manifest === [] || count($manifest) > 200) {
            throw new RuntimeException('Invalid runner package manifest.');
        }
        $offset = 0;
        $files = [];
        while ($offset + 512 <= strlen($archive)) {
            $header = substr($archive, $offset, 512);
            $offset += 512;
            if ($header === str_repeat("\0", 512)) {
                if (substr($archive, $offset) !== str_repeat("\0", 512) || count($files) !== count($manifest)) {
                    throw new RuntimeException('Incomplete runner package.');
                }

                return $files;
            }
            $name = rtrim(substr($header, 0, 100), "\0");
            $checksum = trim(substr($header, 148, 8), "\0 ");
            $sizeText = trim(substr($header, 124, 12), "\0 ");
            $checkedHeader = substr_replace($header, str_repeat(' ', 8), 148, 8);
            if (preg_match('/^[0-7]+$/D', $checksum) !== 1 || octdec($checksum) !== array_sum(unpack('C*', $checkedHeader) ?: [])) {
                throw new RuntimeException('Invalid runner package checksum.');
            }
            if ($header[156] !== '0' || substr($header, 157, 100) !== str_repeat("\0", 100) || substr($header, 345, 155) !== str_repeat("\0", 155)) {
                throw new RuntimeException('Package links, special files and path extensions are forbidden.');
            }
            if (! isset($manifest[$name]) || isset($files[$name]) || preg_match('~^runner/(?:[A-Za-z0-9_-]+/)*[A-Za-z0-9_.-]+$~D', $name) !== 1 || str_contains($name, '..')) {
                throw new RuntimeException('Unexpected or duplicate runner package path.');
            }
            if (preg_match('/^[0-7]+$/D', $sizeText) !== 1 || octdec($sizeText) > 262144) {
                throw new RuntimeException('Runner package file exceeds its limit.');
            }
            $size = (int) octdec($sizeText);
            if ($header !== self::header($name, $size)) {
                throw new RuntimeException('Non-canonical runner package header.');
            }
            $padding = (512 - $size % 512) % 512;
            if (substr($archive, $offset + $size, $padding) !== str_repeat("\0", $padding)) {
                throw new RuntimeException('Nonzero runner package padding.');
            }
            $contents = substr($archive, $offset, $size);
            if (strlen($contents) !== $size || ! hash_equals($manifest[$name], hash('sha256', $contents))) {
                throw new RuntimeException('Runner package file integrity check failed.');
            }
            $files[$name] = $contents;
            $offset += (int) (ceil($size / 512) * 512);
        }
        throw new RuntimeException('Runner package is truncated.');
    }

    private static function header(
        string $path,
        int $size,
    ): string {
        $header = str_pad($path, 100, "\0")
            .sprintf("%07o\0%07o\0%07o\0%011o\0%011o\0", 0600, 0, 0, $size, 0)
            .str_repeat(' ', 8).'0'.str_repeat("\0", 100)
            ."ustar\0".'00'.str_repeat("\0", 247);
        $checksum = array_sum(unpack('C*', $header) ?: []);

        return substr_replace($header, sprintf('%06o', $checksum)."\0 ", 148, 8);
    }

    private static function home(string $home): void
    {
        if (realpath($home) !== $home || ! is_dir($home)) {
            throw new RuntimeException('Use an existing canonical home directory.');
        }
        $current = '';
        foreach (explode('/', trim($home, '/')) as $part) {
            $current .= '/'.$part;
            if (is_link($current)) {
                throw new RuntimeException('Installation paths must not contain symlinks.');
            }
        }
    }

    private static function protect(
        string $path,
        bool $directory,
    ): void {
        $stat = @lstat($path);
        if ($stat === false || ($stat['mode'] & 0170000) !== ($directory ? 0040000 : 0100000) || ($stat['mode'] & 0777) !== ($directory ? 0700 : 0600) || $stat['uid'] !== posix_geteuid() || (! $directory && $stat['nlink'] !== 1)) {
            throw new RuntimeException('Unsafe runner installation ownership, permissions or links.');
        }
    }

    private static function verify(
        string $root,
        array $manifest,
    ): void {
        self::protect($root, true);
        $seen = [];
        foreach (new RecursiveIteratorIterator(new RecursiveDirectoryIterator($root, FilesystemIterator::SKIP_DOTS), RecursiveIteratorIterator::SELF_FIRST) as $entry) {
            $path = $entry->getPathname();
            self::protect($path, $entry->isDir());
            if ($entry->isFile()) {
                $relative = substr($path, strlen($root) + 1);
                if (! isset($manifest[$relative]) || ! hash_equals($manifest[$relative], hash_file('sha256', $path))) {
                    throw new RuntimeException('Existing runner installation was altered. Refusing to reuse it.');
                }
                $seen[] = $relative;
            }
        }
        if (count($seen) !== count($manifest)) {
            throw new RuntimeException('Existing runner installation is incomplete.');
        }
    }

    private static function remove(string $root): void
    {
        if (! is_dir($root)) {
            return;
        }
        foreach (new RecursiveIteratorIterator(new RecursiveDirectoryIterator($root, FilesystemIterator::SKIP_DOTS), RecursiveIteratorIterator::CHILD_FIRST) as $entry) {
            $entry->isDir() && ! $entry->isLink() ? rmdir($entry->getPathname()) : unlink($entry->getPathname());
        }
        rmdir($root);
    }
}
