<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Drivers;

enum NativeFailureStage: string
{
    case VersionInspection = 'version_inspection';
    case AccountProbe = 'account_probe';
    case Preflight = 'preflight';
    case Execution = 'execution';
    case ResultParsing = 'result_parsing';

    public function label(): string
    {
        return match ($this) {
            self::VersionInspection => 'version inspection',
            self::AccountProbe => 'account verification',
            self::Preflight => 'authenticated preflight',
            self::Execution => 'execution',
            self::ResultParsing => 'result validation',
        };
    }
}
