<?php

declare(strict_types=1);

use Shipmunk\Runner\Setup\PackageBuilder;
use Shipmunk\Runner\Setup\PackageInstaller;

require dirname(__DIR__).'/bootstrap.php';

function package_assert(bool $condition): void
{
    if (! $condition) {
        throw new RuntimeException('Runner package assertion failed.');
    }
}

function package_rejects(Closure $operation): void
{
    try {
        $operation();
    } catch (Throwable) {
        return;
    }
    throw new RuntimeException('Expected runner package rejection.');
}

function package_remove(string $path): void
{
    if (is_link($path) || is_file($path)) {
        unlink($path);
    } elseif (is_dir($path)) {
        foreach (new FilesystemIterator($path) as $entry) {
            package_remove($entry->getPathname());
        }
        rmdir($path);
    }
}

function package_rechecksum(string $header): string
{
    $header = substr_replace($header, str_repeat(' ', 8), 148, 8);
    $sum = 0;
    foreach (str_split($header) as $byte) {
        $sum += ord($byte);
    }

    return substr_replace($header, sprintf('%06o', $sum)."\0 ", 148, 8);
}

$root = realpath(sys_get_temp_dir()).'/shipmunk-package-test-'.bin2hex(random_bytes(8));
mkdir($root, 0700);
try {
    $builder = new PackageBuilder;
    $package = $builder->build(dirname(__DIR__, 2));
    $source = $root.'/source';
    foreach ($package['files'] as $path => $hash) {
        if (! is_dir(dirname($source.'/'.$path))) {
            mkdir(dirname($source.'/'.$path), 0700, recursive: true);
        }
        copy(dirname(__DIR__, 2).'/'.$path, $source.'/'.$path);
        touch($source.'/'.$path, 1);
    }
    file_put_contents($source.'/.env', 'SYNTHETIC_PACKAGE_ENV_NEVER_PUBLISH');
    file_put_contents($source.'/runner/setup-secret.json', 'SYNTHETIC_PACKAGE_TOKEN_NEVER_PUBLISH');
    $first = $builder->build($source);
    touch($source.'/runner/bootstrap.php', 1999999999);
    $second = $builder->build($source);
    package_assert($first === $second && $first === $package);
    package_assert(! str_contains($package['contents'], 'SYNTHETIC_PACKAGE_ENV_NEVER_PUBLISH'));
    package_assert(! str_contains($package['contents'], 'SYNTHETIC_PACKAGE_TOKEN_NEVER_PUBLISH'));

    $home = $root.'/home';
    mkdir($home, 0700);
    $release = PackageInstaller::install($package['contents'], $home, $package['hash'], $package['files']);
    foreach ($package['files'] as $path => $hash) {
        package_assert(hash_file('sha256', $release.'/'.$path) === $hash);
        package_assert((fileperms($release.'/'.$path) & 0777) === 0600);
    }
    package_assert(PackageInstaller::install($package['contents'], $home, $package['hash'], $package['files']) === $release);
    fwrite(STDOUT, "PASS deterministic zero-timestamp package excludes synthetic secrets and round-trips every exact file through private installation\n");

    $header = substr($package['contents'], 0, 512);
    $size = (int) octdec(trim(substr($header, 124, 12), "\0 "));
    $entryLength = 512 + (int) (ceil($size / 512) * 512);
    $invalid = [
        'duplicate' => substr($package['contents'], 0, $entryLength).$package['contents'],
        'truncated' => substr($package['contents'], 0, -1),
        'trailing' => $package['contents'].'not-padding',
        'padding' => substr_replace($package['contents'], 'x', 512 + $size, 1),
        'tampered payload' => substr_replace($package['contents'], 'x', 512, 1),
    ];
    foreach (['1', '2', '5', '6', 'x', 'g'] as $type) {
        $invalid['type '.$type] = package_rechecksum(substr_replace($header, $type, 156, 1)).substr($package['contents'], 512);
    }
    foreach (['/outside.php', '../outside.php', 'runner/../outside.php', 'runner\\outside.php', 'runner/unexpected.php'] as $path) {
        $invalid[$path] = package_rechecksum(substr_replace($header, str_pad($path, 100, "\0"), 0, 100)).substr($package['contents'], 512);
    }
    $invalid['magic'] = package_rechecksum(substr_replace($header, 'badmagic', 257, 8)).substr($package['contents'], 512);
    $invalid['size'] = package_rechecksum(substr_replace($header, "77777777777\0", 124, 12)).substr($package['contents'], 512);
    $rejectHome = $root.'/reject';
    mkdir($rejectHome, 0700);
    foreach ($invalid as $archive) {
        package_rejects(fn () => PackageInstaller::install($archive, $rejectHome, hash('sha256', $archive), $package['files']));
        package_assert(! file_exists($rejectHome.'/.shipmunk'));
    }
    package_rejects(fn () => PackageInstaller::install($package['contents'], $rejectHome, str_repeat('0', 64), $package['files']));
    fwrite(STDOUT, "PASS tampered digest, duplicate, truncated, oversized, noncanonical, linked and traversal archives are rejected before filesystem writes\n");

    unlink($source.'/runner/bootstrap.php');
    symlink($source.'/.env', $source.'/runner/bootstrap.php');
    package_rejects(fn () => $builder->build($source));
    unlink($source.'/runner/bootstrap.php');
    copy(dirname(__DIR__).'/bootstrap.php', $source.'/runner/bootstrap.php');
    rename($source.'/runner/containers', $source.'/outside');
    symlink($source.'/outside', $source.'/runner/containers');
    package_rejects(fn () => $builder->build($source));
    fwrite(STDOUT, "PASS package source leaf and ancestor symlinks cannot publish target bytes\n");

    file_put_contents($release.'/runner/bootstrap.php', 'altered');
    package_rejects(fn () => PackageInstaller::install($package['contents'], $home, $package['hash'], $package['files']));
    $linkedHome = $root.'/linked';
    mkdir($linkedHome, 0700);
    symlink($home.'/.shipmunk', $linkedHome.'/.shipmunk');
    package_rejects(fn () => PackageInstaller::install($package['contents'], $linkedHome, $package['hash'], $package['files']));
    fwrite(STDOUT, "PASS altered installed code and redirected installation directories fail closed\n");
} finally {
    package_remove($root);
}
