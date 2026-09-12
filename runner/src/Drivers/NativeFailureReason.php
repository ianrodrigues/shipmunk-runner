<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Drivers;

enum NativeFailureReason: string
{
    case MalformedOutput = 'malformed_output';
    case MissingResult = 'missing_result';
    case InvalidResult = 'invalid_result';
    case ProcessError = 'process_error';
    case AuthExpired = 'auth_expired';
    case RateLimited = 'rate_limited';
    case ApprovalRequired = 'approval_required';

    public function message(): string
    {
        return match ($this) {
            self::MalformedOutput => 'Native runtime returned an unsupported event stream.',
            self::MissingResult => 'Native runtime did not complete a structured result.',
            self::InvalidResult => 'Native runtime returned an invalid structured result.',
            self::ProcessError => 'Native runtime execution failed.',
            self::AuthExpired => 'Native runtime authentication must be renewed.',
            self::RateLimited => 'Native runtime account limit prevented execution.',
            self::ApprovalRequired => 'Native runtime requires approval unavailable in unattended execution.',
        };
    }
}
