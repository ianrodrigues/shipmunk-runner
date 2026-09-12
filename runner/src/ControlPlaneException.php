<?php

declare(strict_types=1);

namespace Shipmunk\Runner;

use RuntimeException;

final class ControlPlaneException extends RuntimeException
{
    public function __construct(public readonly int $status, string $message)
    {
        parent::__construct($message);
    }
}
