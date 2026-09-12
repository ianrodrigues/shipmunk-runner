<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Setup;

use DateTimeImmutable;
use Shipmunk\Runner\Profiles\ProfileStore;

final readonly class SetupBundle
{
    public function __construct(public array $data)
    {
        if (($data['version'] ?? null) !== 1 || ($data['runtime_version'] ?? null) !== '0.154.0') {
            throw new SetupException('Download a setup file for the supported Codex runtime.');
        }
        foreach (['base_url', 'runner_id', 'profile_id', 'expires_at', 'profile_token', 'execution_token'] as $key) {
            if (! is_string($data[$key] ?? null) || $data[$key] === '') {
                throw new SetupException('The setup file is incomplete. Download it again.');
            }
        }
        ProfileStore::identifier($data['runner_id']);
        ProfileStore::identifier($data['profile_id']);
        $url = parse_url($data['base_url']);
        if (! is_array($url)
            || ! in_array($url['scheme'] ?? null, ['http', 'https'], true)
            || ! isset($url['host'])
            || isset($url['pass'])
            || isset($url['user'])
            || isset($url['query'])
            || isset($url['fragment'])
            || preg_match('/[\x00-\x20\x7f\\\\]/', $data['base_url']) === 1
            || filter_var($data['base_url'], FILTER_VALIDATE_URL) === false) {
            throw new SetupException('The server URL must be HTTP(S), without credentials, query, fragment or control characters.');
        }
        if ($url['scheme'] === 'http' && ! in_array(strtolower($url['host']), ['localhost', '127.0.0.1'], true)) {
            throw new SetupException('Use HTTPS for a remote control plane, or HTTP on localhost/127.0.0.1 for local development.');
        }
        foreach (['profile_token', 'execution_token'] as $key) {
            if (preg_match('/^[0-9]+\|[A-Za-z0-9_]{40,160}$/D', $data[$key]) !== 1) {
                throw new SetupException('The setup file contains an invalid runner token.');
            }
        }
        if (preg_match('/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:Z|[+-]\d{2}:\d{2})$/D', $data['expires_at']) !== 1) {
            throw new SetupException('The setup expiry must be an ISO 8601 timestamp.');
        }
        if (new DateTimeImmutable($data['expires_at']) <= new DateTimeImmutable) {
            throw new SetupException('The setup tokens expired. Download setup again for this runner.');
        }
    }

    public static function read(
        string $path,
        ?string $serverUrl = null,
    ): self {
        SetupFiles::canonical(str_starts_with($path, '/') ? $path : getcwd().'/'.$path);
        $stat = @lstat($path);
        if ($stat === false
            || ($stat['mode'] & 0170000) !== 0100000
            || $stat['uid'] !== posix_geteuid()
            || $stat['nlink'] !== 1
            || $stat['size'] > 16384) {
            throw new SetupException('The setup file must be a small regular file owned by this account, without links.');
        }
        if (! chmod($path, 0600)) {
            throw new SetupException('Cannot protect the downloaded setup file.');
        }
        ProfileStore::protect($path);
        $contents = file_get_contents($path, length: 16385);
        $data = json_decode((string) $contents, true, flags: JSON_THROW_ON_ERROR);
        if (! is_array($data)) {
            throw new SetupException('The setup file must contain a JSON object.');
        }

        return new self($serverUrl === null ? $data : [...$data, 'base_url' => $serverUrl]);
    }
}
