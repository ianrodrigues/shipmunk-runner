<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Drivers;

use RuntimeException;
use Shipmunk\Runner\Claim;

final class TrustedAgentInstructions
{
    public function read(
        Claim $claim,
        string $workspace,
    ): string {
        $references = $claim->manifest['instruction_artifacts'] ?? null;
        $configuration = $claim->manifest['effective_config'] ?? [];

        if (! is_array($references) || count($references) !== 1) {
            throw new RuntimeException('Native execution requires one trusted instruction bundle.');
        }

        $bytes = file_get_contents($workspace.'/instructions/0.json', length: 131_073);
        $bundle = is_string($bytes) && strlen($bytes) <= 131_072 ? json_decode($bytes, true) : null;

        if (! is_array($bundle) || ($bundle['version'] ?? null) !== 1
            || ($bundle['trusted_revision'] ?? null) !== ($configuration['trusted_revision'] ?? null)
            || ($bundle['effective_configuration_sha256'] ?? null) !== ($configuration['effective_configuration_sha256'] ?? null)
            || ($bundle['effective_instructions']['contents'] ?? null) !== ($configuration['instructions'] ?? null)
            || ! is_string($bundle['effective_instructions']['contents'] ?? null)
            || ($bundle['effective_instructions']['sha256'] ?? null) !== hash('sha256', $bundle['effective_instructions']['contents'])
            || ($bundle['effective_instructions']['sha256'] ?? null) !== ($configuration['instructions_sha256'] ?? null)) {
            throw new RuntimeException('Trusted native instructions do not match the claim.');
        }

        $agents = $bundle['trusted_files']['AGENTS.md'] ?? null;

        if ($agents === null) {
            return '';
        }

        if (! is_array($agents) || ($agents['path'] ?? null) !== 'AGENTS.md'
            || ! is_string($agents['contents'] ?? null) || strlen($agents['contents']) > 50_000
            || ($agents['sha256'] ?? null) !== hash('sha256', $agents['contents'])) {
            throw new RuntimeException('Trusted AGENTS.md is invalid.');
        }

        return $agents['contents'];
    }
}
