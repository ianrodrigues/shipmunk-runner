<?php

declare(strict_types=1);

use Shipmunk\Runner\CommandRunner;
use Shipmunk\Runner\Profiles\NativeProfile;

require dirname(__DIR__).'/bootstrap.php';

$image = getenv('SHIPMUNK_PROFILE_IMAGE') ?: 'shipmunk-profile-native-test:local';
$commands = new CommandRunner;
$isolated = [
    'docker', 'run', '--rm', '--network', 'none', '--read-only',
    '--user', '65532:65532', '--cap-drop', 'ALL',
    '--security-opt', 'no-new-privileges', '--log-driver', 'none',
    '--tmpfs', '/tmp:rw,nosuid,nodev,size=64m,mode=1777',
    '--entrypoint', '/usr/bin/env', $image,
    '-i', 'PATH=/usr/local/bin:/usr/bin:/bin', 'HOME=/tmp',
];

// Exercise the image's default OpenSSL trust store without contacting an account or provider.
$commands->mustRun([...$isolated, '/bin/sh', '-ec', <<<'SH'
test -s /etc/ssl/certs/ca-certificates.crt || {
    echo 'Native image is missing its system CA bundle.' >&2
    exit 1
}
root=/usr/share/ca-certificates/mozilla/ISRG_Root_X1.crt
openssl verify -no_check_time "$root"
if openssl verify -no_check_time -no-CAfile -no-CApath -no-CAstore "$root" >/dev/null 2>&1; then
    echo 'An untrusted root was unexpectedly accepted.' >&2
    exit 1
fi
SH]);

foreach (NativeProfile::VERSIONS as $agent => $version) {
    $result = $commands->mustRun([...$isolated, ...NativeProfile::command($agent, 'version')]);
    if (! NativeProfile::versionMatches($agent, $version, $result)) {
        throw new RuntimeException('Native image version did not match the pinned runtime.');
    }
}

fwrite(STDOUT, "PASS native image: default system CA trust and pinned CLI versions, with no network or host mounts.\n");
