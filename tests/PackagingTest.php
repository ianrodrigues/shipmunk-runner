<?php

declare(strict_types=1);

$root = dirname(__DIR__);
$temporary = sys_get_temp_dir().'/shipmunk-package-cli-'.bin2hex(random_bytes(8));
mkdir($temporary, 0700);
try {
    foreach (['first', 'second'] as $build) {
        $command = escapeshellarg(PHP_BINARY).' '.escapeshellarg($root.'/tools/package.php').' '.escapeshellarg($temporary.'/'.$build).' '.escapeshellarg('v0.2.0-alpha.1');
        exec($command, $output, $status);
        if ($status !== 0) {
            throw new RuntimeException('Package CLI failed.');
        }
    }
    foreach (['v01.0.0', 'v1.0.0-alpha.01', '../unsafe', 'v1.0.0;echo unsafe'] as $version) {
        $command = escapeshellarg(PHP_BINARY).' '.escapeshellarg($root.'/tools/package.php').' '.escapeshellarg($temporary.'/invalid').' '.escapeshellarg($version).' 2>/dev/null';
        exec($command, $output, $status);
        if ($status === 0 || is_dir($temporary.'/invalid')) {
            throw new RuntimeException('Invalid release tag was accepted.');
        }
    }
    $manifest = json_decode(file_get_contents($temporary.'/first/runner-release.json'), true, flags: JSON_THROW_ON_ERROR);
    $name = 'shipmunk-runner-'.$manifest['version'].'.tar';
    if ($manifest['version'] !== 'v0.2.0-alpha.1') {
        throw new RuntimeException('Explicit release tag was not used.');
    }
    if ($manifest['url'] !== 'https://github.com/ianrodrigues/shipmunk-runner/releases/download/'.$manifest['version'].'/'.$name) {
        throw new RuntimeException('Release manifest does not identify the versioned archive.');
    }
    if ($manifest['sha256'] !== hash_file('sha256', $temporary.'/first/'.$name) || ! isset($manifest['files']['runner/LICENSE'])) {
        throw new RuntimeException('Release manifest does not bind the licensed runtime package.');
    }
    foreach ([$name, 'runner-release.json', 'SHA256SUMS'] as $asset) {
        if (file_get_contents($temporary.'/first/'.$asset) !== file_get_contents($temporary.'/second/'.$asset)) {
            throw new RuntimeException('Release assets are not deterministic.');
        }
    }
    foreach (file($temporary.'/first/SHA256SUMS', FILE_IGNORE_NEW_LINES) as $line) {
        [$hash, $file] = explode('  ', $line, 2);
        if (! in_array($file, [$name, 'runner-release.json'], true) || $hash !== hash_file('sha256', $temporary.'/first/'.$file)) {
            throw new RuntimeException('Published checksum does not match its asset.');
        }
    }
    echo "PASS package CLI produces reproducible licensed archive, versioned manifest and matching checksums\n";
} finally {
    foreach (glob($temporary.'/*/*') as $file) {
        unlink($file);
    }
    foreach (glob($temporary.'/*') as $directory) {
        rmdir($directory);
    }
    rmdir($temporary);
}
