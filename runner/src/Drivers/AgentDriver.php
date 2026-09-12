<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Drivers;

use Closure;
use Shipmunk\Runner\Claim;
use Shipmunk\Runner\NativeExecution;

interface AgentDriver
{
    /**
     * @param  Closure(int): void  $checkpoint
     * @return array{version: string, structured_output: bool, explicit_resume: bool}
     */
    public function inspect(
        AgentTransport $transport,
        Closure $checkpoint,
    ): array;

    /**
     * @param  Closure(int): void  $checkpoint
     */
    public function probe(
        AgentTransport $transport,
        Closure $checkpoint,
    ): void;

    /**
     * @param  Closure(int): void  $checkpoint
     */
    public function start(
        Claim $claim,
        AgentTransport $transport,
        Closure $checkpoint,
        ?AgentSession $session = null,
    ): NativeExecution;

    public function stop(AgentTransport $transport): void;
}
