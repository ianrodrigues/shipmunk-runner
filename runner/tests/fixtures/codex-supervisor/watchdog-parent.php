<?php

declare(strict_types=1);

use Shipmunk\Runner\Drivers\CodexWatchdog;

require dirname(__DIR__, 3).'/bootstrap.php';

$name = $argv[1] ?? throw new RuntimeException('Missing sandbox name.');
$ready = $argv[2] ?? throw new RuntimeException('Missing readiness path.');
$lease = (new CodexWatchdog)->arm(
    $name,
    new DateTimeImmutable('+45 seconds'),
    new DateTimeImmutable('+60 seconds'),
);
file_put_contents($ready, 'armed');

while (true) {
    sleep(1);
}
