<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Drivers;

use InvalidArgumentException;
use RuntimeException;

final class DriverFailure extends RuntimeException
{
    public function __construct(public readonly string $reason)
    {
        $message = match ($reason) {
            'malformed_output' => 'Native runtime returned an unsupported event stream.',
            'missing_result' => 'Native runtime did not complete a structured result.',
            'invalid_result' => 'Native runtime returned an invalid structured result.',
            'process_error' => 'Native runtime execution failed.',
            'auth_expired' => 'Native runtime authentication must be renewed.',
            'rate_limited' => 'Native runtime account limit prevented execution.',
            'approval_required' => 'Native runtime requires approval unavailable in unattended execution.',
            default => throw new InvalidArgumentException('Unsupported native failure reason.'),
        };

        parent::__construct($message);
    }
}
