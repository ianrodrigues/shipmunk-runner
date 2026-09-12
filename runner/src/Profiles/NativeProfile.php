<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Profiles;

use RuntimeException;
use Shipmunk\Runner\CommandResult;
use Shipmunk\Runner\Drivers\CodexEventParser;
use Shipmunk\Runner\Drivers\DriverFailure;
use Shipmunk\Runner\Drivers\NativeFailureReason;

final class NativeProfile
{
    public const array VERSIONS = ['codex' => '0.154.0', 'claude_code' => '2.1.269'];

    /** Select stable identity fields in a canonical order for strict comparisons. */
    public static function identity(array $binding): array
    {
        return [
            'profile_id' => $binding['profile_id'] ?? null,
            'credential_reference' => $binding['credential_reference'] ?? null,
            'agent' => $binding['agent'] ?? null,
            'auth_mode' => $binding['auth_mode'] ?? null,
            'runtime_version' => $binding['runtime_version'] ?? null,
        ];
    }

    /** @return list<string> */
    public static function command(string $agent, string $command): array
    {
        return match ([$agent, $command]) {
            ['codex', 'version'] => ['/usr/local/bin/codex', '--version'],
            ['codex', 'login'] => ['/usr/local/bin/codex', 'login', '--device-auth'],
            ['codex', 'preflight'] => [
                '/usr/local/bin/codex', 'exec', '--json', '--ephemeral', '--ignore-user-config', '--ignore-rules',
                '--skip-git-repo-check', '--sandbox', 'read-only',
                '-c', 'forced_login_method="chatgpt"', '-c', 'cli_auth_credentials_store="file"',
                '-c', 'features.shell_tool=false', '-c', 'features.unified_exec=false',
                '-c', 'web_search="disabled"',
                'Reply exactly SHIPMUNK_AUTH_OK. Do not use tools or inspect files.',
            ],
            ['claude_code', 'preflight'] => [
                '/usr/local/bin/claude', '-p', '--output-format', 'json', '--safe-mode',
                '--tools', '', '--strict-mcp-config', '--no-session-persistence', '--max-turns', '1',
                '--settings', '{"forceLoginMethod":"claudeai"}',
                'Reply exactly SHIPMUNK_AUTH_OK. Do not use tools or inspect files.',
            ],
            ['codex', 'probe'] => ['/usr/local/bin/codex', 'login', 'status'],
            ['claude_code', 'version'] => ['/usr/local/bin/claude', '--version'],
            ['claude_code', 'login'] => ['/usr/local/bin/claude', 'auth', 'login', '--claudeai'],
            ['claude_code', 'probe'] => ['/usr/local/bin/claude', 'auth', 'status'],
            default => throw new RuntimeException('Unsupported native profile command.'),
        };
    }

    public static function versionMatches(string $agent, string $expected, CommandResult $result): bool
    {
        $actual = trim($result->stdout);

        return $result->exitCode === 0 && $expected === (self::VERSIONS[$agent] ?? null)
            && $actual === match ($agent) {
                'codex' => 'codex-cli '.$expected,
                'claude_code' => $expected.' (Claude Code)',
                default => '',
            };
    }

    /** @return array{health: string, reason: ?string} */
    public static function health(string $agent, CommandResult $result): array
    {
        if ($result->exitCode !== 0) {
            return ['health' => 'expired', 'reason' => 'native_login_required'];
        }
        if ($agent === 'codex' && trim($result->stdout.$result->stderr) === 'Logged in using ChatGPT') {
            return ['health' => 'ready', 'reason' => null];
        }
        if ($agent === 'claude_code') {
            $status = json_decode($result->stdout, true);
            if (is_array($status) && ($status['loggedIn'] ?? null) === true
                && ($status['authMethod'] ?? null) === 'claude.ai'
                && ($status['apiProvider'] ?? null) === 'firstParty'
                && in_array($status['subscriptionType'] ?? null, ['pro', 'max', 'team', 'enterprise'], true)) {
                return ['health' => 'ready', 'reason' => null];
            }
        }

        return ['health' => 'unsupported', 'reason' => 'native_mode_mismatch'];
    }

    /** A cached auth status alone must never mark a profile ready. */
    public static function authenticatedHealth(string $agent, CommandResult $result): array
    {
        $failed = ['health' => 'error', 'reason' => 'native_probe_failed'];
        if (! in_array($agent, ['codex', 'claude_code'], true)) {
            return $failed;
        }
        if ($agent === 'claude_code') {
            if ($result->exitCode !== 0) {
                return $failed;
            }
            $data = json_decode($result->stdout, true);

            return is_array($data) && ($data['type'] ?? null) === 'result'
                && ($data['subtype'] ?? null) === 'success' && ($data['is_error'] ?? null) === false
                && is_string($data['result'] ?? null) && trim($data['result']) === 'SHIPMUNK_AUTH_OK'
                ? ['health' => 'ready', 'reason' => null] : $failed;
        }

        try {
            $reason = (new CodexEventParser)->preflightFailureReason(
                $result->exitCode,
                $result->stdout,
                $result->stderr,
            );
        } catch (DriverFailure) {
            return $failed;
        }

        if ($reason === NativeFailureReason::RateLimited) {
            return ['health' => 'rate_limited', 'reason' => 'rate_limited'];
        }

        return $reason === null ? ['health' => 'ready', 'reason' => null] : $failed;
    }

    public static function initializeHome(string $home): void
    {
        $old = umask(0077);
        try {
            foreach (['.codex', '.codex/tmp', '.claude', '.config', '.cache'] as $directory) {
                if (! mkdir($home.'/'.$directory, 0700)) {
                    throw new RuntimeException('Cannot create native configuration directory.');
                }
            }
            $configuration = [
                '/.codex/config.toml' => "cli_auth_credentials_store = \"file\"\nforced_login_method = \"chatgpt\"\n",
                '/.claude/settings.json' => '{"forceLoginMethod":"claudeai"}',
            ];
            foreach ($configuration as $path => $contents) {
                if (file_put_contents($home.$path, $contents) !== strlen($contents)) {
                    throw new RuntimeException('Cannot write native configuration file.');
                }
            }
        } finally {
            umask($old);
        }
    }
}
