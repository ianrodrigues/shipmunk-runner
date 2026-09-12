<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

interface ControlPlaneClient
{
    public function claim(): ?Claim;

    public function heartbeat(Claim $claim): Heartbeat;

    /** Confirm that the old sandbox and its writable workspace no longer exist. */
    public function acknowledgeStopped(Claim $claim): void;

    /** @param list<array<string, mixed>> $events */
    public function sendEvents(Claim $claim, array $events): void;

    public function downloadArtifact(Claim $claim, string $artifactId, string $sha256): string;

    public function uploadArtifact(Claim $claim, string $kind, string $bytes, string $sha256): string;

    /** @param array<string, mixed> $result */
    public function complete(Claim $claim, array $result): void;
}
