<?php

declare(strict_types=1);

use Shipmunk\Runner\Claim;
use Shipmunk\Runner\ControlPlaneClient;
use Shipmunk\Runner\Heartbeat;
use Shipmunk\Runner\SafeTarExtractor;
use Shipmunk\Runner\WorkspacePreparer;

require dirname(__DIR__).'/bootstrap.php';

function source_assert(
    bool $condition,
    string $message,
): void {
    if (! $condition) {
        throw new RuntimeException($message);
    }
}

function source_directory(): string
{
    $path = sys_get_temp_dir().'/shipmunk-source-test-'.bin2hex(random_bytes(6));
    mkdir($path, 0700);

    return $path;
}

function source_remove(string $path): void
{
    $entries = new RecursiveIteratorIterator(
        new RecursiveDirectoryIterator($path, FilesystemIterator::SKIP_DOTS),
        RecursiveIteratorIterator::CHILD_FIRST,
    );

    foreach ($entries as $entry) {
        $entry->isDir() && ! $entry->isLink()
            ? rmdir($entry->getPathname())
            : unlink($entry->getPathname());
    }

    rmdir($path);
}

function source_entry(
    string $name,
    string $contents = '',
    string $type = '0',
    int $mode = 0644,
): string {
    $header = str_pad($name, 100, "\0")
        .sprintf('%07o', $mode)."\0"
        .sprintf('%07o', 65532)."\0"
        .sprintf('%07o', 65532)."\0"
        .sprintf('%011o', strlen($contents))."\0"
        .sprintf('%011o', 0)."\0"
        .str_repeat(' ', 8)
        .$type
        .str_repeat("\0", 100)
        ."ustar\0"
        .'00'
        .str_repeat("\0", 247);
    $header = substr_replace($header, sprintf('%06o', array_sum(unpack('C*', $header)))."\0 ", 148, 8);

    return $header.$contents.str_repeat("\0", (512 - strlen($contents) % 512) % 512);
}

function source_pax(
    string $key,
    string $value,
): string {
    $body = ' '.$key.'='.$value."\n";
    $length = strlen($body) + 1;

    while (strlen((string) $length) + strlen($body) !== $length) {
        $length = strlen((string) $length) + strlen($body);
    }

    return $length.$body;
}

function source_reject(
    string $archive,
    ?SafeTarExtractor $extractor = null,
): void {
    $directory = source_directory();

    try {
        try {
            ($extractor ?? new SafeTarExtractor)->extract($archive, $directory);
        } catch (RuntimeException) {
            source_assert(scandir($directory) === ['.', '..'], 'Rejected metadata wrote a file.');

            return;
        }

        throw new LogicException('Unsafe archive was accepted.');
    } finally {
        source_remove($directory);
    }
}

/**
 * @param  list<string>  $archives
 * @param  array<string, mixed>  $manifest
 * @return array{WorkspacePreparer, Claim, ControlPlaneClient}
 */
function source_preparer(
    string $root,
    array $archives,
    array $manifest,
): array {
    $references = [];
    $artifacts = ['instructions' => '{}'];

    foreach ($archives as $index => $archive) {
        $id = 'source-'.$index;
        $artifacts[$id] = $archive;
        $references[] = [
            'artifact_id' => $id,
            'sha256' => hash('sha256', $archive),
        ];
    }

    $client = new class($artifacts) implements ControlPlaneClient
    {
        public function __construct(private array $artifacts) {}

        public function downloadArtifact(
            Claim $claim,
            string $artifactId,
            string $sha256,
        ): string {
            $bytes = $this->artifacts[$artifactId];
            source_assert(hash('sha256', $bytes) === $sha256, 'Fixture artifact digest mismatch.');

            return $bytes;
        }

        public function claim(): ?Claim
        {
            throw new LogicException('Unexpected control-plane call.');
        }

        public function heartbeat(Claim $claim): Heartbeat
        {
            throw new LogicException('Unexpected control-plane call.');
        }

        public function acknowledgeStopped(Claim $claim): void
        {
            throw new LogicException('Unexpected control-plane call.');
        }

        public function sendEvents(
            Claim $claim,
            array $events,
        ): void {
            throw new LogicException('Unexpected control-plane call.');
        }

        public function uploadArtifact(
            Claim $claim,
            string $kind,
            string $bytes,
            string $sha256,
        ): string {
            throw new LogicException('Unexpected control-plane call.');
        }

        public function complete(
            Claim $claim,
            array $result,
        ): void {
            throw new LogicException('Unexpected control-plane call.');
        }
    };
    $claim = new Claim(
        '01kkkkkkkkkkkkkkkkkkkkkkkk',
        '01nnnnnnnnnnnnnnnnnnnnnnnn',
        1,
        new DateTimeImmutable('+45 seconds'),
        new DateTimeImmutable('+10 minutes'),
        array_merge($manifest, [
            'protocol_version' => '1.0',
            'source_artifacts' => $references,
            'instruction_artifacts' => [[
                'artifact_id' => 'instructions',
                'sha256' => hash('sha256', '{}'),
            ]],
        ]),
    );

    return [new WorkspacePreparer($root), $claim, $client];
}

$fixture = json_decode(file_get_contents(__DIR__.'/fixtures/source-archive/git-archives.json'), true, flags: JSON_THROW_ON_ERROR);
source_assert($fixture['git_version'] === 'git version 2.39.5', 'Unexpected source fixture provenance.');
$archives = [];

foreach (['short', 'full', 'executable', 'subtree', 'long'] as $name) {
    $archives[$name] = base64_decode($fixture[$name], true);
}

$root = source_directory();

try {
    [$preparer, $claim, $client] = source_preparer($root, [$archives['short'], $archives['long']], [
        'diff_base_sha' => $fixture['base_sha'],
        'head_sha' => $fixture['head_sha'],
    ]);
    $checkpoints = 0;
    $workspace = $preparer->prepare($claim, $client, static function () use (&$checkpoints): void {
        $checkpoints++;
    });

    source_assert(file_get_contents($workspace.'/sources/0/src/index.txt') === "fixture\n", 'Base snapshot wrapper was not removed.');
    source_assert(file_get_contents($workspace.'/sources/1/'.str_repeat('x', 120)) === "long path\n", 'Long PAX path was not extracted into the head root.');
    source_assert((fileperms($workspace.'/sources/0/run.sh') & 07777) === 0700, 'Executable owner mode was lost.');
    source_assert((fileperms($workspace.'/sources/0/src/index.txt') & 07777) === 0600, 'Regular file permissions are too broad.');
    source_assert(scandir($workspace.'/sources') === ['.', '..', '0', '1'], 'Archive normalization left staging data.');
    source_assert($checkpoints >= 8, 'Source preparation did not expose cancellation checkpoints.');
    $preparer->remove($workspace);
} finally {
    source_remove($root);
}
fwrite(STDOUT, "PASS git commit PAX metadata, long paths, base/head wrappers and executable mode.\n");

foreach ([
    [$archives['full'], $fixture['base_sha'], 'run.sh'],
    [$archives['short'], str_repeat('a', 40), 'owner-demo-'.substr($fixture['base_sha'], 0, 7).'/run.sh'],
    [$archives['subtree'], $fixture['base_sha'], 'src/index.txt'],
] as [$archive, $sha, $path]) {
    $root = source_directory();

    try {
        [$preparer, $claim, $client] = source_preparer($root, [$archive], ['head_sha' => $sha]);
        $workspace = $preparer->prepare($claim, $client);
        source_assert(is_file($workspace.'/sources/0/'.$path), 'Source wrapper matching altered an unrelated directory.');
        $preparer->remove($workspace);
    } finally {
        source_remove($root);
    }
}

$wrapper = 'owner-demo-'.substr($fixture['base_sha'], 0, 7);
$nested = source_entry($wrapper.'/', type: '5')
    .source_entry($wrapper.'/'.$wrapper.'/', type: '5')
    .source_entry($wrapper.'/'.$wrapper.'/keep.txt', 'kept')
    .str_repeat("\0", 1024);
$root = source_directory();

try {
    [$preparer, $claim, $client] = source_preparer($root, [$nested], ['head_sha' => $fixture['base_sha']]);
    $workspace = $preparer->prepare($claim, $client);
    source_assert(file_get_contents($workspace.'/sources/0/'.$wrapper.'/keep.txt') === 'kept', 'Wrapper normalization collided with a repository directory.');
    $preparer->remove($workspace);
} finally {
    source_remove($root);
}
fwrite(STDOUT, "PASS full-SHA wrappers normalize while unrelated and same-name repository directories survive.\n");

foreach (['../outside', '/absolute', 'safe/../outside', 'safe//outside', 'safe\\outside', '', "safe\0outside"] as $path) {
    source_reject(source_entry('extended', source_pax('path', $path), 'x').source_entry('placeholder', 'unsafe').str_repeat("\0", 1024));
}

foreach ([
    ['g', source_pax('path', 'outside')],
    ['x', source_pax('linkpath', 'outside')],
    ['x', source_pax('size', '999')],
    ['x', source_pax('GNU.sparse.size', '999')],
    ['x', source_pax('path', 'first').source_pax('path', 'second')],
    ['x', '0 path=safe'."\n"],
    ['x', '999999 path=safe'."\n"],
    ['x', '013 path=safe'."\n"],
    ['x', '12 path=safe'],
    ['x', ''],
    ['x', str_repeat('x', 65_537)],
] as [$type, $payload]) {
    source_reject(source_entry('extended', $payload, $type).source_entry('placeholder', 'unsafe').str_repeat("\0", 1024));
}

source_reject(source_entry('extended', source_pax('path', 'safe'), 'x').str_repeat("\0", 1024));
source_reject(source_entry('extended', source_pax('path', 'safe'), 'x').source_entry('second', source_pax('path', 'other'), 'x').str_repeat("\0", 1024));
source_reject(source_entry('extended', source_pax('comment', 'bounded'), 'g').str_repeat("\0", 1024), new SafeTarExtractor(maxBytes: 1));
source_reject(source_entry('extended', source_pax('comment', 'bounded'), 'g').source_entry('again', source_pax('comment', 'bounded'), 'g').str_repeat("\0", 1024), new SafeTarExtractor(maxFiles: 1));

foreach ([['directory//', '5'], ['regular/', '0'], ['link', '2']] as [$name, $type]) {
    source_reject(source_entry($name, type: $type).str_repeat("\0", 1024));
}
fwrite(STDOUT, "PASS PAX traversal, unsupported metadata, malformed lengths, limits, links and unsafe trailing slashes fail closed.\n");

foreach ([
    04755 => 0700,
    06644 => 0600,
    00700 => 0700,
    00711 => 0700,
    00610 => 0600,
    00601 => 0600,
    00010 => 0600,
] as $mode => $expected) {
    $directory = source_directory();

    try {
        (new SafeTarExtractor)->extract(source_entry('mode', 'data', mode: $mode).str_repeat("\0", 1024), $directory);
        source_assert((fileperms($directory.'/mode') & 07777) === $expected, 'Archive preserved privilege or non-owner permission bits.');
    } finally {
        source_remove($directory);
    }
}
fwrite(STDOUT, "PASS source permissions retain only owner access and executable intent.\n");

$root = source_directory();

try {
    [$preparer, $claim, $client] = source_preparer($root, [$archives['short']], ['head_sha' => $fixture['base_sha']]);
    $checkpoints = 0;

    try {
        $preparer->prepare($claim, $client, static function () use (&$checkpoints): void {
            if (++$checkpoints === 4) {
                throw new RuntimeException('Cancellation fixture.');
            }
        });
        throw new LogicException('Cancellation did not stop source preparation.');
    } catch (RuntimeException $exception) {
        source_assert($exception->getMessage() === 'Cancellation fixture.', 'Unexpected preparation failure.');
        source_assert(! file_exists($preparer->path($claim)), 'Cancelled preparation left staged source data.');
    }
} finally {
    source_remove($root);
}
fwrite(STDOUT, "PASS cancellation removes the normalized attempt workspace.\n");
