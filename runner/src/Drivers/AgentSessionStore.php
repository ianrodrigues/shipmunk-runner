<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Drivers;

use RuntimeException;
use Shipmunk\Runner\Claim;
use Shipmunk\Runner\Profiles\ProfileStore;

final readonly class AgentSessionStore
{
    public function __construct(private string $root) {}

    public function read(Claim $claim): ?AgentSession
    {
        $path = $this->path($claim);

        if (! file_exists($path)
            && ! is_link($path)) {
            return null;
        }

        ProfileStore::protect($path);
        $json = file_get_contents($path, length: 1025);
        $data = is_string($json) && strlen($json) <= 1024 ? json_decode($json, true) : null;

        if (! is_array($data)
            || ! is_string($data['id'] ?? null)
            || ! is_string($data['binding'] ?? null)) {
            return null;
        }

        $session = new AgentSession($data['id'], $data['binding']);

        return $session->compatible($claim) ? $session : null;
    }

    public function write(
        Claim $claim,
        AgentSession $session,
    ): void {
        if (! $session->compatible($claim)) {
            throw new RuntimeException('Cannot persist an incompatible native session.');
        }

        $path = $this->path($claim);
        $temporary = $path.'.'.bin2hex(random_bytes(8));
        $json = json_encode([
            'id' => $session->id,
            'binding' => $session->binding,
        ], JSON_THROW_ON_ERROR);
        $mask = umask(0077);

        try {
            $file = fopen($temporary, 'xb');
        } finally {
            umask($mask);
        }

        if ($file === false) {
            throw new RuntimeException('Cannot create native session record.');
        }

        try {
            if (fwrite($file, $json) !== strlen($json)
                || ! fflush($file)
                || ! fsync($file)
                || ! rename($temporary, $path)) {
                throw new RuntimeException('Cannot persist native session record.');
            }
        } finally {
            fclose($file);

            if (is_file($temporary)) {
                unlink($temporary);
            }
        }
    }

    private function path(Claim $claim): string
    {
        if (! str_starts_with($this->root, '/')
            || in_array('..', explode('/', $this->root), true)
            || in_array('.', explode('/', $this->root), true)) {
            throw new RuntimeException('Native session directory must be an absolute, canonical path.');
        }

        $walked = '';

        foreach (explode('/', trim($this->root, '/')) as $part) {
            $walked .= '/'.$part;

            if (is_link($walked)) {
                throw new RuntimeException('Native session directory cannot contain a symlink.');
            }
        }

        if (! is_dir($this->root)
            && ! mkdir($this->root, 0700, true)) {
            throw new RuntimeException('Cannot create native session directory.');
        }

        ProfileStore::protect($this->root, true);

        return $this->root.'/'.$claim->runId.'.json';
    }
}
