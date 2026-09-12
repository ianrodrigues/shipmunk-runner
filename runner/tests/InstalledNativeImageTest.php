<?php

declare(strict_types=1);

use Shipmunk\Runner\CommandRunner;
use Shipmunk\Runner\Setup\PackageBuilder;
use Shipmunk\Runner\Setup\PackageInstaller;

require dirname(__DIR__).'/bootstrap.php';

function installed_image_assert(bool $condition): void
{
    if (! $condition) {
        throw new RuntimeException('Installed native image assertion failed.');
    }
}

function installed_image_remove(string $path): void
{
    if (is_dir($path) && ! is_link($path)) {
        foreach (new FilesystemIterator($path) as $entry) {
            installed_image_remove($entry->getPathname());
        }
        rmdir($path);

        return;
    }

    unlink($path);
}

$image = getenv('SHIPMUNK_PROFILE_IMAGE') ?: 'shipmunk-profile-native-test:local';
$repository = dirname(__DIR__, 2);
$root = realpath(sys_get_temp_dir()).'/shipmunk-installed-image-'.bin2hex(random_bytes(8));
$assets = [
    'runner/containers/codex-mcp.mjs',
    'runner/containers/codex-result.schema.json',
];
mkdir($root, 0700);

try {
    $package = (new PackageBuilder)->build($repository);
    $release = PackageInstaller::install(
        $package['contents'],
        $root,
        $package['hash'],
        $package['files'],
    );

    foreach ($assets as $asset) {
        installed_image_assert((fileperms($release.'/'.$asset) & 0777) === 0600);
    }

    $commands = new CommandRunner;
    $commands->mustRun([
        'env',
        'DOCKER_BUILDKIT=1',
        'docker',
        'build',
        '--pull=false',
        '--tag',
        $image,
        '--file',
        $release.'/runner/containers/Dockerfile',
        $release,
    ], timeoutSeconds: 600);

    $modes = $commands->mustRun([
        'docker',
        'run',
        '--rm',
        '--network',
        'none',
        '--read-only',
        '--entrypoint',
        '/usr/bin/stat',
        $image,
        '-c',
        '%a:%u:%g',
        '/usr/local/lib/shipmunk',
        '/usr/local/lib/shipmunk/codex-mcp.mjs',
        '/usr/local/lib/shipmunk/codex-result.schema.json',
    ])->stdout;
    installed_image_assert(trim($modes) === "755:0:0\n644:0:0\n644:0:0");

    $isolated = [
        'docker',
        'run',
        '--rm',
        '--network',
        'none',
        '--read-only',
        '--user',
        '65532:65532',
        '--cap-drop',
        'ALL',
        '--security-opt',
        'no-new-privileges',
        '--log-driver',
        'none',
        '--entrypoint',
        '/usr/bin/env',
        $image,
        '-i',
        'PATH=/usr/local/bin:/usr/bin:/bin',
        '/usr/local/bin/node',
        '-e',
        <<<'JS'
const fs = require('node:fs');
fs.accessSync('/usr/local/lib/shipmunk/codex-mcp.mjs', fs.constants.R_OK);
JSON.parse(fs.readFileSync('/usr/local/lib/shipmunk/codex-result.schema.json', 'utf8'));
JS,
    ];
    $commands->mustRun($isolated);

    fwrite(STDOUT, "PASS baked schema and MCP assets remain readable by the unprivileged agent after private installation\n");
} finally {
    installed_image_remove($root);
}
