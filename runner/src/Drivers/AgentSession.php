<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Drivers;

use Shipmunk\Runner\Claim;

/**
 * A trusted host record, never selected from provider-global history or repository input.
 */
final readonly class AgentSession
{
    public function __construct(
        public string $id,
        public string $binding,
    ) {}

    public function compatible(Claim $claim): bool
    {
        return preg_match('/^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$/D', $this->id) === 1
            && hash_equals(self::binding($claim), $this->binding);
    }

    public static function binding(Claim $claim): string
    {
        $manifest = $claim->manifest;

        return hash('sha256', json_encode([
            'run_id' => $claim->runId,
            'repository_id' => $manifest['repository_id'] ?? null,
            'base_sha' => $manifest['base_sha'] ?? null,
            'head_sha' => $manifest['head_sha'] ?? null,
            'profile_id' => $manifest['profile_id'] ?? null,
            'credential_reference' => $manifest['supervisor']['credential_reference'] ?? null,
            'agent' => $manifest['agent'] ?? null,
            'runtime_version' => $manifest['runtime_version'] ?? null,
            'effective_config' => $manifest['effective_config'] ?? null,
            'task_context' => $manifest['task_context'] ?? null,
            'instruction_artifacts' => $manifest['instruction_artifacts'] ?? null,
            'source_artifacts' => $manifest['source_artifacts'] ?? null,
        ], JSON_THROW_ON_ERROR));
    }
}
