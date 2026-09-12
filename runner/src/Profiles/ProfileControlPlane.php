<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Profiles;

interface ProfileControlPlane
{
    /** @return array<string, mixed> */
    public function request(string $profileId, string $suffix, array $payload): array;
}
