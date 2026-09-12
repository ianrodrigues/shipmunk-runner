<?php

declare(strict_types=1);

use Shipmunk\Runner\Setup\PackageBuilder;

require dirname(__DIR__).'/runner/bootstrap.php';

$root = dirname(__DIR__);
$version = $argv[2] ?? trim(file_get_contents($root.'/VERSION'));
if (preg_match('/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/D', $version) !== 1) {
    throw new RuntimeException('Package version must be a semantic version tag beginning with v.');
}
$output = $argv[1] ?? $root.'/dist';
if (! is_dir($output) && ! mkdir($output, 0755, recursive: true)) {
    throw new RuntimeException('Cannot create package output directory.');
}
if (is_link($output) || realpath($output) === false) {
    throw new RuntimeException('Package output must be a regular directory.');
}
$package = (new PackageBuilder)->build($root);
$name = 'shipmunk-runner-'.$version.'.tar';
$entrypoint = file_get_contents($root.'/tools/installer-entrypoint.php');
if (! str_starts_with($entrypoint, "<?php\n")) {
    throw new RuntimeException('Invalid installer entrypoint template.');
}
$installer = file_get_contents($root.'/runner/src/Setup/PackageInstaller.php')."\n".substr($entrypoint, 6);
$manifest = json_encode([
    'version' => $version,
    'url' => 'https://github.com/ianrodrigues/shipmunk-runner/releases/download/'.$version.'/'.$name,
    'sha256' => $package['hash'],
    'installer_url' => 'https://github.com/ianrodrigues/shipmunk-runner/releases/download/'.$version.'/installer.php',
    'installer_sha256' => hash('sha256', $installer),
    'files' => $package['files'],
], JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES | JSON_THROW_ON_ERROR)."\n";
$assets = [$name => $package['contents'], 'installer.php' => $installer, 'runner-release.json' => $manifest];
$checksums = '';
foreach ($assets as $path => $contents) {
    $checksums .= hash('sha256', $contents).'  '.$path."\n";
}
$assets['SHA256SUMS'] = $checksums;
foreach ($assets as $path => $contents) {
    if (file_put_contents($output.'/'.$path, $contents) !== strlen($contents)) {
        throw new RuntimeException('Cannot write package output.');
    }
}
echo $version.' '.$package['hash']."\n";
