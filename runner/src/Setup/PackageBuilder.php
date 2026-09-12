<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Setup;

use FilesystemIterator;
use RecursiveDirectoryIterator;
use RecursiveIteratorIterator;
use RuntimeException;

final class PackageBuilder
{
    /**
     * @return array{contents: string, hash: string, files: array<string, string>}
     */
    public function build(string $root): array
    {
        $paths = [
            'runner/LICENSE',
            'runner/bootstrap.php',
            'runner/setup.php',
            'runner/bin/shipmunk-setup',
            'runner/bin/shipmunk-runner',
            'runner/bin/shipmunk-profile',
            'runner/containers/Dockerfile',
            'runner/containers/Dockerfile.dockerignore',
            'runner/containers/codex-mcp.mjs',
            'runner/containers/codex-result.schema.json',
        ];
        $root = rtrim($root, '/');
        foreach (new RecursiveIteratorIterator(new RecursiveDirectoryIterator($root.'/runner/src', FilesystemIterator::SKIP_DOTS)) as $file) {
            if ($file->getExtension() === 'php') {
                $paths[] = substr($file->getPathname(), strlen($root) + 1);
            }
        }
        sort($paths);

        $archive = '';
        $files = [];
        foreach ($paths as $path) {
            $source = $root.'/'.$path;
            if (realpath($source) !== realpath($root).'/'.$path) {
                throw new RuntimeException('Runner package source path must not contain symlinks.');
            }
            $stat = lstat($source);
            if ($stat === false || ($stat['mode'] & 0170000) !== 0100000 || $stat['nlink'] !== 1) {
                throw new RuntimeException('Runner package source must be a regular unlinked file.');
            }

            $contents = file_get_contents($source);
            if ($contents === false || strlen($contents) > 262144) {
                throw new RuntimeException('Runner package source exceeds its limit.');
            }

            $files[$path] = hash('sha256', $contents);
            $archive .= $this->header($path, strlen($contents));
            $archive .= $contents.str_repeat("\0", (512 - strlen($contents) % 512) % 512);
        }
        $archive .= str_repeat("\0", 1024);
        if (strlen($archive) > 2097152) {
            throw new RuntimeException('Runner package exceeds its limit.');
        }

        return [
            'contents' => $archive,
            'hash' => hash('sha256', $archive),
            'files' => $files,
        ];
    }

    private function header(
        string $path,
        int $size,
    ): string {
        if (strlen($path) >= 100) {
            throw new RuntimeException('Runner package path exceeds its limit.');
        }

        // A deterministic, regular-file-only ustar subset: no links, directories or extensions.
        $header = str_pad($path, 100, "\0")
            .sprintf("%07o\0%07o\0%07o\0%011o\0%011o\0", 0600, 0, 0, $size, 0)
            .str_repeat(' ', 8).'0'.str_repeat("\0", 100)
            ."ustar\0".'00'.str_repeat("\0", 247);
        $checksum = array_sum(unpack('C*', $header) ?: []);

        return substr_replace($header, sprintf('%06o', $checksum)."\0 ", 148, 8);
    }
}
