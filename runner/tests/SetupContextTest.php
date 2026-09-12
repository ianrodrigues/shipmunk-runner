<?php

declare(strict_types=1);

// This scratch export exercises Docker's actual context filtering with synthetic secrets.
// It does not build a native runtime or contact an account.
$root = realpath(sys_get_temp_dir()).'/shipmunk-context-'.bin2hex(random_bytes(8));
$context = $root.'/input';
$output = $root.'/output';
mkdir($context.'/runner/containers', 0700, recursive: true);
file_put_contents($context.'/.env', 'SYNTHETIC_CONTEXT_ENV_MUST_NOT_LEAVE_CHECKOUT');
file_put_contents($context.'/runner/containers/nested-setup.json', 'SYNTHETIC_NESTED_SETUP_TOKEN');
file_put_contents($context.'/shipmunk-setup.json', 'SYNTHETIC_CONTEXT_TOKEN_MUST_NOT_LEAVE_CHECKOUT');
file_put_contents($context.'/runner/containers/Dockerfile', "FROM scratch\nCOPY . /\n");
foreach (['Dockerfile.dockerignore', 'codex-mcp.mjs', 'codex-result.schema.json'] as $name) {
    copy(dirname(__DIR__).'/containers/'.$name, $context.'/runner/containers/'.$name);
}
try {
    $process = proc_open(['docker', 'build', '--file', $context.'/runner/containers/Dockerfile', '--output', 'type=local,dest='.$output, $context], [0 => ['pipe', 'r'], 1 => ['file', $root.'/build.log', 'w'], 2 => ['file', $root.'/build.log', 'a']], $pipes);
    if (! is_resource($process)) {
        throw new RuntimeException('Cannot test Docker context isolation.');
    }
    fclose($pipes[0]);
    if (proc_close($process) !== 0) {
        throw new RuntimeException('Docker context isolation build failed.');
    }
    $paths = [];
    foreach (new RecursiveIteratorIterator(new RecursiveDirectoryIterator($output, FilesystemIterator::SKIP_DOTS)) as $entry) {
        $paths[] = substr($entry->getPathname(), strlen($output) + 1);
    }
    sort($paths);
    if ($paths !== ['runner/containers/codex-mcp.mjs', 'runner/containers/codex-result.schema.json']) {
        throw new RuntimeException('Native image build context contains unexpected files: '.json_encode($paths));
    }
    fwrite(STDOUT, "PASS actual Docker context excludes synthetic app environment and setup tokens, while retaining required runtime assets\n");
} finally {
    $entries = new RecursiveIteratorIterator(new RecursiveDirectoryIterator($root, FilesystemIterator::SKIP_DOTS), RecursiveIteratorIterator::CHILD_FIRST);
    foreach ($entries as $entry) {
        $entry->isDir() ? rmdir($entry->getPathname()) : unlink($entry->getPathname());
    }
    rmdir($root);
}
