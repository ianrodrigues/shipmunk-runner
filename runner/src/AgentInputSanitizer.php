<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

final class AgentInputSanitizer
{
    /** @var list<string> */
    private const array FORBIDDEN_KEYS = [
        'supervisor',
        'authorization',
        'callback',
        'credential_reference',
        'runner_token',
        'github_token',
        'app_key',
        'database_url',
    ];

    /**
     * @param  array<string, mixed>  $manifest
     * @return array<string, mixed>
     */
    public function sanitize(array $manifest): array
    {
        $clean = [];

        foreach ($manifest as $key => $value) {
            if (in_array(strtolower($key), self::FORBIDDEN_KEYS, true)) {
                continue;
            }

            $clean[$key] = is_array($value) ? $this->filter($value) : $value;
        }

        return $clean;
    }

    /**
     * @param  array<array-key, mixed>  $input
     * @return array<array-key, mixed>
     */
    private function filter(array $input): array
    {
        $clean = [];

        foreach ($input as $key => $value) {
            if (is_string($key) && in_array(strtolower($key), self::FORBIDDEN_KEYS, true)) {
                continue;
            }

            $clean[$key] = is_array($value) ? $this->filter($value) : $value;
        }

        return $clean;
    }
}
