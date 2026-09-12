<?php

declare(strict_types=1);

use Shipmunk\Runner\Setup\SetupBundle;
use Shipmunk\Runner\Setup\SetupFiles;
use Shipmunk\Runner\Setup\SetupWizard;

require dirname(__DIR__).'/bootstrap.php';

function setup_assert(bool $condition): void
{
    if (! $condition) {
        throw new RuntimeException('Guided setup assertion failed.');
    }
}

function setup_rejects(Closure $operation): void
{
    try {
        $operation();
    } catch (Throwable) {
        return;
    }
    throw new RuntimeException('Expected unsafe setup rejection.');
}

function setup_remove(string $path): void
{
    if (is_link($path) || is_file($path)) {
        unlink($path);
    } elseif (is_dir($path)) {
        foreach (new FilesystemIterator($path) as $entry) {
            setup_remove($entry->getPathname());
        }
        rmdir($path);
    }
}

// Synthetic tokens and local process fixtures only; no native accounts or Docker calls.
$data = [
    'version' => 1,
    'base_url' => 'https://shipmunk.example',
    'runner_id' => '01kkkkkkkkkkkkkkkkkkkkkkkk',
    'profile_id' => '01mmmmmmmmmmmmmmmmmmmmmmmm',
    'runtime_version' => '0.154.0',
    'expires_at' => '2099-01-01T00:00:00+00:00',
    'profile_token' => '1|shipmunk_runner_'.str_repeat('A', 40),
    'execution_token' => '2|shipmunk_runner_'.str_repeat('B', 40),
];
$root = realpath(sys_get_temp_dir()).'/shipmunk-setup-test-'.bin2hex(random_bytes(8));
mkdir($root, 0700);
try {
    foreach ([
        'https://user:password@shipmunk.example',
        'https://user@shipmunk.example',
        "https://shipmunk.example/\nSECRET",
        "https://shipmunk.example/\x1b[2J",
        'https://shipmunk.example?token=SECRET',
        'https://shipmunk.example/#fragment',
        'http://remote.example',
        'file:///tmp/credentials',
        'https://shipmunk.example\\@evil.example',
    ] as $url) {
        setup_rejects(fn () => new SetupBundle([...$data, 'base_url' => $url]));
    }
    foreach ([
        ['expires_at' => '2000-01-01T00:00:00Z'],
        ['expires_at' => "2099-01-01T00:00:00Z\n"],
        ['profile_id' => '../../other-profile'],
        ['profile_token' => "1|SYNTHETIC\r\nInjected: value"],
        ['runtime_version' => '0.1.0'],
        ['version' => 2],
    ] as $invalid) {
        setup_rejects(fn () => new SetupBundle([...$data, ...$invalid]));
    }
    new SetupBundle([...$data, 'base_url' => 'http://127.0.0.1:8001']);
    fwrite(STDOUT, "PASS malformed, expired, remote-HTTP and terminal-control bundles fail closed\n");

    file_put_contents($root.'/bundle.json', json_encode($data));
    chmod($root.'/bundle.json', 0644);
    $bundle = SetupBundle::read($root.'/bundle.json');
    setup_assert(SetupBundle::read($root.'/bundle.json', 'https://runner-reachable.example')->data['base_url'] === 'https://runner-reachable.example');
    setup_rejects(fn () => SetupBundle::read($root.'/bundle.json', 'http://remote.example'));
    setup_rejects(fn () => SetupBundle::read($root.'/bundle.json', 'https://user:SECRET@example.com'));
    setup_assert((fileperms($root.'/bundle.json') & 0777) === 0600);
    symlink($root.'/bundle.json', $root.'/linked.json');
    setup_rejects(fn () => SetupBundle::read($root.'/linked.json'));
    link($root.'/bundle.json', $root.'/hard.json');
    setup_rejects(fn () => SetupBundle::read($root.'/hard.json'));
    unlink($root.'/hard.json');
    mkdir($root.'/real', 0700);
    symlink($root.'/real', $root.'/redirect');
    file_put_contents($root.'/real/bundle.json', json_encode($data));
    setup_rejects(fn () => SetupBundle::read($root.'/redirect/bundle.json'));
    setup_rejects(fn () => new SetupFiles($root.'/redirect', $data['runner_id']));
    setup_rejects(fn () => new SetupFiles($root.'/real/..', $data['runner_id']));
    fwrite(STDOUT, "PASS downloaded files reject hard links, final/intermediate symlinks and traversal\n");

    $files = new SetupFiles($root, $data['runner_id']);
    $files->install($bundle, dirname(__DIR__, 2), PHP_BINARY);
    file_put_contents($root.'/untouched', 'unchanged');
    unlink($files->root.'/execution.token');
    symlink($root.'/untouched', $files->root.'/execution.token');
    setup_rejects(fn () => $files->install($bundle, dirname(__DIR__, 2), PHP_BINARY));
    setup_assert(file_get_contents($root.'/untouched') === 'unchanged');
    unlink($files->root.'/execution.token');
    $files->install($bundle, dirname(__DIR__, 2), PHP_BINARY);
    setup_rejects(fn () => $files->install(new SetupBundle([...$data, 'base_url' => 'https://other.example']), dirname(__DIR__, 2), PHP_BINARY));
    setup_assert(json_decode(file_get_contents($files->root.'/config.json'), true)['base_url'] === $data['base_url']);
    fwrite(STDOUT, "PASS renewal rejects redirected token files and changed server/profile identity\n");

    $first = SetupWizard::operationId();
    $second = SetupWizard::operationId();
    setup_assert($first !== $second && preg_match('/^[0-7][0-9a-hjkmnp-tv-z]{25}$/D', $first) === 1);
    try {
        SetupWizard::checkServer('http://127.0.0.1:1');
        throw new RuntimeException('Expected unreachable server rejection.');
    } catch (Shipmunk\Runner\Setup\SetupException $exception) {
        setup_assert(str_contains($exception->getMessage(), 'server health check failed'));
        setup_assert(! str_contains($exception->getMessage(), '127.0.0.1'));
    }
    fwrite(STDOUT, "PASS connection IDs are fresh lowercase ULIDs\n");
} finally {
    setup_remove($root);
}
