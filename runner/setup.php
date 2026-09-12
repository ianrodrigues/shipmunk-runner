<?php

declare(strict_types=1);

use Shipmunk\Runner\Setup\SetupException;
use Shipmunk\Runner\Setup\SetupWizard;

require __DIR__.'/bootstrap.php';

try {
    if (PHP_VERSION_ID < 80500 || ! extension_loaded('pcntl') || ! extension_loaded('posix')) {
        throw new SetupException('Host PHP 8.5+ with pcntl and posix is required. Set PHP_BIN to that PHP executable, then retry.');
    }
    exit((new SetupWizard(dirname(__DIR__)))->run(array_slice($argv, 1)));
} catch (Throwable $exception) {
    // Only setup-owned errors are safe to print; parsing and runtime errors may contain secrets.
    fwrite(STDERR, $exception instanceof SetupException ? $exception->getMessage()."\n" : "Setup failed. Check the file and prerequisites, then retry.\n");
    exit(1);
}
