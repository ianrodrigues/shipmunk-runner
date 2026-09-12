<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

final class Protocol
{
    /** Matches the versioned JSON Schemas in contracts/v1/. */
    public const string VERSION = '1.0';

    /** Matches the server-side input artifact limit. */
    public const int INPUT_ARTIFACT_MAX_BYTES = 100 * 1024 * 1024;

    /** Keeps any single control-plane operation below the heartbeat cadence. */
    public const int HTTP_TIMEOUT_SECONDS = 5;
}
