<?php

declare(strict_types=1);

use Shipmunk\Runner\Claim;
use Shipmunk\Runner\Drivers\AgentSession;
use Shipmunk\Runner\Drivers\AgentSessionStore;

require dirname(__DIR__).'/bootstrap.php';

function session_store_rejects(
    Closure $operation,
    string $message,
): void {
    try {
        $operation();
    } catch (RuntimeException $exception) {
        if ($exception->getMessage() === $message) {
            return;
        }

        throw $exception;
    }

    throw new RuntimeException('Expected session path rejection.');
}

$root = realpath(sys_get_temp_dir()).'/shipmunk-session-path-'.bin2hex(random_bytes(8));
mkdir($root, 0700);
mkdir($root.'/target', 0700);
symlink($root.'/target', $root.'/redirect');
$claim = new Claim(
    '01k4w000000000000000000001',
    '01k4w000000000000000000002',
    1,
    new DateTimeImmutable('2099-01-01T00:00:45Z'),
    new DateTimeImmutable('2099-01-01T00:30:00Z'),
    [
        'protocol_version' => '1.0',
    ],
);
$session = new AgentSession('0199a213-81c0-7800-8aa1-bbab2a035a53', AgentSession::binding($claim));

try {
    foreach ([$root.'/redirect/sessions', $root.'/redirect'] as $path) {
        $store = new AgentSessionStore($path);
        session_store_rejects(fn () => $store->read($claim), 'Native session directory cannot contain a symlink.');
        session_store_rejects(fn () => $store->write($claim, $session), 'Native session directory cannot contain a symlink.');
    }

    if (scandir($root.'/target') !== ['.', '..']) {
        throw new RuntimeException('Rejected session path modified the symlink target.');
    }

    fwrite(STDOUT, "PASS session reads and writes reject intermediate and final symlinks without creating redirected files\n");

    foreach (['relative/sessions', $root.'/target/../sessions', $root.'/./sessions'] as $path) {
        $store = new AgentSessionStore($path);
        session_store_rejects(fn () => $store->read($claim), 'Native session directory must be an absolute, canonical path.');
        session_store_rejects(fn () => $store->write($claim, $session), 'Native session directory must be an absolute, canonical path.');
    }

    fwrite(STDOUT, "PASS session paths reject relative and traversal components before filesystem changes\n");

    $store = new AgentSessionStore($root.'/safe,sessions');
    $store->write($claim, $session);

    if ($store->read($claim)?->id !== $session->id) {
        throw new RuntimeException('Protected canonical session directory did not round-trip its record.');
    }

    fwrite(STDOUT, "PASS canonical session paths preserve compatible records without Docker-specific filename restrictions\n");
} finally {
    $record = $root.'/safe,sessions/'.$claim->runId.'.json';

    if (is_file($record)) {
        unlink($record);
    }

    if (is_dir($root.'/safe,sessions')) {
        rmdir($root.'/safe,sessions');
    }

    unlink($root.'/redirect');
    rmdir($root.'/target');
    rmdir($root);
}

fwrite(STDOUT, "3 session storage scenarios passed with real filesystem permissions and symlinks.\n");
