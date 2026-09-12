<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Setup;

use Shipmunk\Runner\Profiles\ProfileStore;
use Shipmunk\Runner\StreamHttpTransport;
use Throwable;

final readonly class SetupWizard
{
    public function __construct(private string $checkout) {}

    public function run(array $arguments): int
    {
        if (in_array($arguments[0] ?? '', ['run', 'connect'], true)) {
            return $this->launch($arguments);
        }
        if (! in_array(count($arguments), [1, 2], true) || in_array($arguments[0], ['--help', '-h'], true)) {
            fwrite(STDOUT, "Usage: bash runner/bin/shipmunk-setup /path/to/shipmunk-setup-RUNNER.json [--server-url=https://runner-reachable-server]\nRun as the non-root runner account from a Shipmunk checkout. Requires PHP 8.5+, pcntl, posix, and a Linux Docker engine.\n");

            return count($arguments) === 1 ? 0 : 2;
        }
        if (posix_geteuid() === 0) {
            throw new SetupException('Run setup as a dedicated non-root account with Docker access.');
        }
        if (! stream_isatty(STDIN)) {
            throw new SetupException('Guided setup requires an operator terminal.');
        }

        $serverUrl = null;
        if (isset($arguments[1])) {
            if (! str_starts_with($arguments[1], '--server-url=')) {
                throw new SetupException('The only setup option is --server-url=https://runner-reachable-server.');
            }
            $serverUrl = substr($arguments[1], strlen('--server-url='));
        }

        $bundle = SetupBundle::read($arguments[0], $serverUrl);
        $home = getenv('HOME');
        if (! is_string($home) || $home === '') {
            throw new SetupException('The runner account needs a home directory.');
        }
        fwrite(STDOUT, 'Server: '.$bundle->data['base_url']."\nRunner: ".$bundle->data['runner_id']."\nProfile: ".$bundle->data['profile_id']."\nTokens expire: ".$bundle->data['expires_at']."\n");
        fwrite(STDOUT, "Setup installs private local files and builds the pinned runtime image. It does not start reviews.\n");
        if (! $this->confirm('Continue with this server and runner?')) {
            return 0;
        }

        $this->docker();
        self::checkServer($bundle->data['base_url']);
        $files = new SetupFiles($home, $bundle->data['runner_id']);
        fwrite(STDOUT, "Building the pinned runtime image…\n");
        $image = $this->buildImage($files);
        $files->install($bundle, $this->checkout, PHP_BINARY, $image);
        fwrite(STDOUT, "Installed private configuration bound to the built image. The downloaded setup file is mode 0600; delete it after setup.\n");

        fwrite(STDOUT, "Codex login stays in this terminal. Its authenticated probe can consume subscription usage; use the account designated for this runner.\n");
        $result = 0;
        if ($this->confirm('Run native Codex login now?')) {
            $result = $this->launch(['connect', $files->root]);
        }

        fwrite(STDOUT, "\nCommands for this runner:\n  Login/retry: bash ".escapeshellarg($files->root.'/connect')."\n  Probe:       bash ".escapeshellarg($files->root.'/connect')." probe\n  Poll once:   bash ".escapeshellarg($files->root.'/run')." --once\n  Run:         bash ".escapeshellarg($files->root.'/run')."\n\nStart polling only after granting this profile access to your repository. Polling processes already queued work.\nWhen tokens expire, download setup again for this same runner and rerun setup. Existing profile and recovery state are retained.\n");

        return $result;
    }

    public static function checkServer(string $baseUrl): void
    {
        try {
            $health = (new StreamHttpTransport)->request(
                'GET',
                rtrim($baseUrl, '/').'/up',
                ['Accept' => 'application/json'],
                '',
                5,
                32768,
            );
            if ($health->status === 200) {
                return;
            }
        } catch (Throwable) {
            // DNS, TLS and connection errors must not expose transport text or response bodies.
        }
        throw new SetupException('The server health check failed. Check its address and connectivity, then retry. No runner files were installed.');
    }

    private function confirm(string $message): bool
    {
        fwrite(STDOUT, $message.' [y/N] ');

        return strtolower(trim((string) fgets(STDIN))) === 'y';
    }

    private function docker(): void
    {
        $process = proc_open(
            ['docker', 'version', '--format', '{{.Server.Os}}'],
            [
                0 => ['pipe', 'r'],
                1 => ['pipe', 'w'],
                2 => ['pipe', 'w'],
            ],
            $pipes,
        );
        if (! is_resource($process)) {
            throw new SetupException('Install Docker and start its Linux engine, then retry setup.');
        }
        fclose($pipes[0]);
        $os = trim((string) stream_get_contents($pipes[1]));
        fclose($pipes[1]);
        fclose($pipes[2]);
        if (proc_close($process) !== 0 || $os !== 'linux') {
            throw new SetupException('Start a Linux Docker engine accessible to this account. Docker Desktop on macOS can provide it.');
        }
    }

    private function launch(array $arguments): int
    {
        [$command, $root] = $arguments + [null, null];
        if (! is_string($root) || realpath($root) !== $root) {
            throw new SetupException('The runner directory must be canonical. Rerun setup if it moved.');
        }

        SetupFiles::canonical($root);
        ProfileStore::protect($root, true);
        ProfileStore::protect($root.'/config.json');
        $config = json_decode((string) file_get_contents($root.'/config.json'), true, flags: JSON_THROW_ON_ERROR);
        ProfileStore::protect($root.'/profile.token');
        ProfileStore::protect($root.'/execution.token');
        $bundle = new SetupBundle([
            ...$config,
            'version' => 1,
            'runtime_version' => '0.154.0',
            'profile_token' => trim((string) file_get_contents($root.'/profile.token')),
            'execution_token' => trim((string) file_get_contents($root.'/execution.token')),
        ]);
        $image = $config['image_id'] ?? null;
        if (! is_string($image) || preg_match('/\Asha256:[a-f0-9]{64}\z/', $image) !== 1) {
            throw new SetupException('Rerun setup to bind this installation to its built runtime image.');
        }
        $base = [
            '--base-url='.$bundle->data['base_url'],
            '--profiles-dir='.$root.'/profiles',
            '--image='.$image,
        ];
        if ($command === 'run') {
            if (count($arguments) > 3 || (isset($arguments[2]) && $arguments[2] !== '--once')) {
                throw new SetupException('The runner command accepts only --once.');
            }
            $options = [
                ...$base,
                '--token-file='.$root.'/execution.token',
                '--state-dir='.$root.'/state',
                '--driver=codex',
                '--repository-image='.$image,
            ];
            if (isset($arguments[2])) {
                $options[] = '--once';
            }

            return $this->execute([PHP_BINARY, $this->checkout.'/runner/bin/shipmunk-runner', ...$options]);
        }

        $operation = $arguments[2] ?? 'login';
        if (count($arguments) > 3 || ! in_array($operation, ['login', 'probe'], true)) {
            throw new SetupException('The connection command accepts only login or probe.');
        }
        if (! stream_isatty(STDIN) || ! stream_isatty(STDOUT) || ! stream_isatty(STDERR)) {
            throw new SetupException('Native connection operations require an operator terminal.');
        }
        // ProfileLifecycle recovers its original pending operation before starting this new ID.
        // Token renewal never writes or replaces that journal.
        $options = [
            ...$base,
            '--token-file='.$root.'/profile.token',
            '--profile='.$bundle->data['profile_id'],
            '--operation='.$operation,
            '--operation-id='.self::operationId(),
        ];

        return $this->execute([PHP_BINARY, $this->checkout.'/runner/bin/shipmunk-profile', ...$options]);
    }

    public function buildImage(SetupFiles $files): string
    {
        $identity = hash_init('sha256');
        foreach (['Dockerfile', 'codex-mcp.mjs', 'codex-result.schema.json'] as $asset) {
            if (! hash_update_file($identity, $this->checkout.'/runner/containers/'.$asset)) {
                throw new SetupException('Runtime image inputs are missing. Download the runner again.');
            }
        }
        $tag = 'shipmunk-profile-native:'.hash_final($identity);
        $iid = $files->root.'/image-'.bin2hex(random_bytes(8)).'.tmp';
        try {
            $built = $this->execute(['env', 'DOCKER_BUILDKIT=1', 'docker', 'build', '--tag', $tag, '--iidfile', $iid, '--file', $this->checkout.'/runner/containers/Dockerfile', $this->checkout]);
            if ($built !== 0) {
                throw new SetupException('Runtime image build failed. Existing configuration is unchanged; rerun setup to retry.');
            }
            $image = trim((string) file_get_contents($iid));
            if (preg_match('/\Asha256:[a-f0-9]{64}\z/', $image) !== 1) {
                throw new SetupException('Docker did not return an immutable runtime image identity.');
            }

            return $image;
        } finally {
            if (is_file($iid)) {
                unlink($iid);
            }
        }
    }

    public static function operationId(): string
    {
        $timestamp = (int) floor(microtime(true) * 1000);
        $bytes = substr(pack('J', $timestamp), 2).random_bytes(10);
        $bits = '00';
        foreach (str_split($bytes) as $byte) {
            $bits .= str_pad(decbin(ord($byte)), 8, '0', STR_PAD_LEFT);
        }
        $id = '';
        foreach (str_split($bits, 5) as $part) {
            $id .= '0123456789abcdefghjkmnpqrstvwxyz'[bindec($part)];
        }

        return $id;
    }

    private function execute(array $command): int
    {
        $process = proc_open($command, [
            0 => STDIN,
            1 => STDOUT,
            2 => STDERR,
        ], $pipes, $this->checkout);
        if (! is_resource($process)) {
            throw new SetupException('Cannot start the runner command. Check PHP and Docker prerequisites.');
        }

        return proc_close($process);
    }
}
