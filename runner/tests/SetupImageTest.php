<?php

declare(strict_types=1);

use Shipmunk\Runner\CommandRunner;
use Shipmunk\Runner\Setup\SetupBundle;
use Shipmunk\Runner\Setup\SetupFiles;
use Shipmunk\Runner\Setup\SetupWizard;

require dirname(__DIR__).'/bootstrap.php';

function image_assert(bool $condition): void
{
    if (! $condition) {
        throw new RuntimeException('Installation image identity assertion failed.');
    }
}

function image_remove(string $path): void
{
    if (is_dir($path) && ! is_link($path)) {
        foreach (new FilesystemIterator($path) as $entry) {
            image_remove($entry->getPathname());
        }
        rmdir($path);
    } else {
        unlink($path);
    }
}

$root = realpath(sys_get_temp_dir()).'/shipmunk-image-'.bin2hex(random_bytes(8));
mkdir($root, 0700);
$images = [];
$tags = [];
$commands = new CommandRunner;
try {
    foreach (['first', 'second'] as $index => $version) {
        $checkout = $root.'/'.$version;
        mkdir($checkout.'/runner/containers', 0700, recursive: true);
        mkdir($checkout.'/runner/bin', 0700);
        file_put_contents($checkout.'/runner/containers/Dockerfile', "FROM alpine:3.20\nLABEL shipmunk.synthetic.version=".$version."\nLABEL shipmunk.synthetic.fixture=".basename($root)."\n");
        foreach (['codex-mcp.mjs', 'codex-result.schema.json'] as $asset) {
            file_put_contents($checkout.'/runner/containers/'.$asset, 'SYNTHETIC_IMAGE_'.$version);
        }
        copy(dirname(__DIR__).'/containers/Dockerfile.dockerignore', $checkout.'/runner/containers/Dockerfile.dockerignore');
        file_put_contents($checkout.'/runner/bin/shipmunk-runner', '<?php file_put_contents(__DIR__."/captured.json", json_encode($argv));');
        $bundle = new SetupBundle([
            'version' => 1, 'runtime_version' => '0.154.0', 'base_url' => 'https://shipmunk.example',
            'runner_id' => '01'.str_repeat($index === 0 ? 'k' : 'm', 24),
            'profile_id' => '01'.str_repeat('n', 24), 'expires_at' => '2099-01-01T00:00:00Z',
            'profile_token' => '1|'.str_repeat('A', 40), 'execution_token' => '2|'.str_repeat('B', 40),
        ]);
        $files = new SetupFiles($root, $bundle->data['runner_id']);
        $wizard = new SetupWizard($checkout);
        $image = $wizard->buildImage($files);
        $images[] = $image;
        $tags = [...$tags, ...json_decode($commands->mustRun(['docker', 'image', 'inspect', '--format', '{{json .RepoTags}}', $image])->stdout, true)];
        $files->install($bundle, $checkout, PHP_BINARY, $image);
        if ($index === 0) {
            $first = [$wizard, $files, $checkout];
        }
    }
    image_assert($images[0] !== $images[1]);
    [$wizard, $files, $checkout] = $first;
    image_assert($wizard->run(['run', $files->root, '--once']) === 0);
    $arguments = json_decode(file_get_contents($checkout.'/runner/bin/captured.json'), true);
    image_assert(in_array('--image='.$images[0], $arguments, true));
    image_assert(in_array('--repository-image='.$images[0], $arguments, true));
    image_assert(trim($commands->mustRun(['docker', 'image', 'inspect', '--format', '{{index .Config.Labels "shipmunk.synthetic.version"}}', $images[0]])->stdout) === 'first');
    $commands->mustRun(['docker', 'run', '--rm', '--network', 'none', $images[0], 'true']);
    fwrite(STDOUT, "PASS two built installations retain distinct immutable images and restarting the first uses its original image for both operations\n");
} finally {
    foreach (array_unique($tags) as $tag) {
        $commands->run(['docker', 'image', 'rm', $tag]);
    }
    image_remove($root);
}
