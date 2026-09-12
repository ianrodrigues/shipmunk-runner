<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Drivers;

use RuntimeException;

final class DriverFailure extends RuntimeException
{
    public readonly ?int $exitCode;

    public function __construct(
        public readonly NativeFailureReason $reason,
        public readonly ?NativeFailureStage $stage = null,
        ?int $exitCode = null,
    ) {
        if (
            $exitCode !== null
            && ($exitCode < 0 || $exitCode > 255)
        ) {
            $exitCode = null;
        }

        $this->exitCode = $exitCode;

        parent::__construct($reason->message());
    }

    public function summary(): string
    {
        if ($this->stage === null) {
            return $this->getMessage();
        }

        return $this->getMessage()
            .' Stage: '.$this->stage->label().'.'
            .' Reason: '.$this->reason->value.'.'
            .' Native exit code: '.($this->exitCode ?? 'unavailable').'.';
    }
}
