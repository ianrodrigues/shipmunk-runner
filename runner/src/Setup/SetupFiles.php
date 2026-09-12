<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Setup;

use RuntimeException;
use Shipmunk\Runner\Profiles\ProfileStore;

final readonly class SetupFiles
{
    public string $root;

    public function __construct(
        string $home,
        string $runner,
    ) {
        try {
            ProfileStore::identifier($runner);
        } catch (RuntimeException $exception) {
            throw new SetupException('Unsafe setup identifier, ownership, type or permissions.', previous: $exception);
        }
        if (realpath($home) !== $home || ! is_dir($home)) {
            throw new SetupException('The home directory must be an existing canonical directory.');
        }
        self::canonical($home);
        foreach (['/.shipmunk', '/.shipmunk/runners', '/.shipmunk/runners/'.$runner] as $suffix) {
            self::directory($home.$suffix);
        }
        $this->root = $home.'/.shipmunk/runners/'.$runner;
        self::directory($this->root.'/profiles');
        self::directory($this->root.'/state');
    }

    public static function canonical(string $path): void
    {
        if (! str_starts_with($path, '/') || str_contains($path, '//') || preg_match('~/\.\.?(?:/|$)~', $path) === 1) {
            throw new SetupException('Setup paths must be canonical, without traversal components.');
        }
        $current = '';
        foreach (explode('/', trim($path, '/')) as $part) {
            $current .= '/'.$part;
            if (is_link($current)) {
                throw new SetupException('Symlinks are forbidden in setup paths.');
            }
        }
    }

    private static function directory(string $path): void
    {
        if (! file_exists($path) && ! is_link($path) && ! mkdir($path, 0700)) {
            throw new SetupException('Cannot create the private runner directory.');
        }
        try {
            ProfileStore::protect($path, true);
        } catch (RuntimeException $exception) {
            throw new SetupException('Unsafe setup identifier, ownership, type or permissions.', previous: $exception);
        }
    }

    public function write(
        string $name,
        string $contents,
    ): void {
        if (! in_array($name, ['profile.token', 'execution.token', 'config.json', 'run', 'connect'], true)) {
            throw new SetupException('Unexpected setup filename.');
        }
        $path = $this->root.'/'.$name;
        if (file_exists($path) || is_link($path)) {
            try {
                ProfileStore::protect($path);
            } catch (RuntimeException $exception) {
                throw new SetupException('Unsafe setup identifier, ownership, type or permissions.', previous: $exception);
            }
        }
        $temporary = $path.'.'.bin2hex(random_bytes(8));
        $old = umask(0077);
        $file = fopen($temporary, 'x');
        umask($old);
        if ($file === false) {
            throw new SetupException('Cannot write private runner configuration.');
        }
        try {
            if (fwrite($file, $contents) !== strlen($contents) || ! fflush($file) || ! fsync($file)) {
                throw new SetupException('Cannot persist runner configuration.');
            }
            if (! rename($temporary, $path)) {
                throw new SetupException('Cannot activate runner configuration.');
            }
        } finally {
            fclose($file);
            if (is_file($temporary)) {
                unlink($temporary);
            }
        }
    }

    public function install(
        SetupBundle $bundle,
        string $checkout,
        string $php,
        string $image,
    ): void {
        if (preg_match('/\Asha256:[a-f0-9]{64}\z/', $image) !== 1) {
            throw new SetupException('Setup requires an immutable runtime image identity.');
        }
        $config = [
            'image_id' => $image,
            'base_url' => $bundle->data['base_url'],
            'runner_id' => $bundle->data['runner_id'],
            'profile_id' => $bundle->data['profile_id'],
            'expires_at' => $bundle->data['expires_at'],
        ];
        $configFile = $this->root.'/config.json';
        if (file_exists($configFile) || is_link($configFile)) {
            try {
                ProfileStore::protect($configFile);
            } catch (RuntimeException $exception) {
                throw new SetupException('Unsafe setup identifier, ownership, type or permissions.', previous: $exception);
            }
            $previous = json_decode((string) file_get_contents($configFile), true, flags: JSON_THROW_ON_ERROR);
            foreach (['base_url', 'runner_id', 'profile_id'] as $key) {
                if (($previous[$key] ?? null) !== $config[$key]) {
                    throw new SetupException('Setup identity differs from this existing runner. Keep its recovery state and use the original server/profile.');
                }
            }
        }
        $this->write('profile.token', $bundle->data['profile_token']);
        $this->write('execution.token', $bundle->data['execution_token']);
        $this->write('config.json', json_encode($config, JSON_THROW_ON_ERROR));
        foreach (['run', 'connect'] as $command) {
            $arguments = [$php, $checkout.'/runner/setup.php', $command, $this->root];
            $this->write($command, "#!/usr/bin/env bash\nexec ".implode(' ', array_map(escapeshellarg(...), $arguments)).' "$@"'."\n");
        }
    }
}
