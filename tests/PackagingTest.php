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
    foreach ([$name, 'installer.php', 'runner-release.json', 'SHA256SUMS'] as $asset) {
        if (file_get_contents($temporary.'/first/'.$asset) !== file_get_contents($temporary.'/second/'.$asset)) {
            throw new RuntimeException('Release assets are not deterministic.');
        }
    }
    foreach (file($temporary.'/first/SHA256SUMS', FILE_IGNORE_NEW_LINES) as $line) {
        [$hash, $file] = explode('  ', $line, 2);
        if (! in_array($file, [$name, 'installer.php', 'runner-release.json'], true) || $hash !== hash_file('sha256', $temporary.'/first/'.$file)) {
            throw new RuntimeException('Published checksum does not match its asset.');
        }
    }
    if ($manifest['installer_sha256'] !== hash_file('sha256', $temporary.'/first/installer.php') || $manifest['installer_url'] !== 'https://github.com/ianrodrigues/shipmunk-runner/releases/download/'.$manifest['version'].'/installer.php') {
        throw new RuntimeException('Release manifest does not bind the installer asset.');
    }
    $home = $temporary.'/home';
    mkdir($home, 0700);
    $arguments = [PHP_BINARY, $temporary.'/first/installer.php', $temporary.'/first/'.$name, realpath($home), $manifest['sha256'], base64_encode(json_encode($manifest['files'], JSON_THROW_ON_ERROR))];
    $command = implode(' ', array_map(escapeshellarg(...), $arguments));
    exec($command, $installed, $status);
    if ($status !== 0 || count($installed) !== 1) {
        throw new RuntimeException('Published installer failed to install the published archive.');
    }
    foreach ($manifest['files'] as $path => $hash) {
        if (hash_file('sha256', $installed[0].'/'.$path) !== $hash) {
            throw new RuntimeException('Published installer produced different runtime bytes.');
        }
    }
    $badHome = $temporary.'/rejected-home';
    mkdir($badHome, 0700);
    $arguments[3] = realpath($badHome);
    $arguments[4] = str_repeat('0', 64);
    exec(implode(' ', array_map(escapeshellarg(...), $arguments)).' 2>/dev/null', $output, $status);
    if ($status === 0 || file_exists($badHome.'/.shipmunk')) {
        throw new RuntimeException('Published installer accepted a mismatched archive digest.');
    }
    echo "PASS package CLI produces reproducible licensed assets and the verified installer installs exact files while rejecting a bad digest\n";
} finally {
    foreach (new RecursiveIteratorIterator(new RecursiveDirectoryIterator($temporary, FilesystemIterator::SKIP_DOTS), RecursiveIteratorIterator::CHILD_FIRST) as $entry) {
        $entry->isDir() ? rmdir($entry->getPathname()) : unlink($entry->getPathname());
    }
    rmdir($temporary);
}
