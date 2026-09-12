<?php

try {
    if (PHP_SAPI !== 'cli' || PHP_VERSION_ID < 80500 || ! function_exists('posix_geteuid')) {
        throw new \RuntimeException('Runner installation requires PHP 8.5 CLI with posix.');
    }
    if ($argc !== 5 || strlen($argv[4]) > 65536) {
        throw new \RuntimeException('Expected archive, home, archive SHA-256 and bounded base64 file manifest.');
    }
    $json = base64_decode($argv[4], true);
    if ($json === false) {
        throw new \RuntimeException('Invalid installer file manifest.');
    }
    $manifest = json_decode($json, true, flags: JSON_THROW_ON_ERROR);
    if (! is_array($manifest)) {
        throw new \RuntimeException('Invalid installer file manifest.');
    }
    if (! is_file($argv[1]) || is_link($argv[1])) {
        throw new \RuntimeException('Runner archive must be a regular file.');
    }
    $archive = file_get_contents($argv[1], length: 2097153);
    if ($archive === false) {
        throw new \RuntimeException('Cannot read runner archive.');
    }
    $release = \Shipmunk\Runner\Setup\PackageInstaller::install($archive, $argv[2], $argv[3], $manifest);
    fwrite(STDOUT, $release."\n");
} catch (\Throwable $exception) {
    $message = $exception::class === \RuntimeException::class
        ? $exception->getMessage()
        : 'Runner installation failed. Check the setup download and private installation directory.';
    fwrite(STDERR, $message."\n");
    exit(1);
}
