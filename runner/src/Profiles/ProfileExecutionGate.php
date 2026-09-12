<?php

declare(strict_types=1);

namespace Shipmunk\Runner\Profiles;

use Closure;
use RuntimeException;

/**
 * Native execution integrations must hold this gate for the whole execution/refresh.
 */
final class ProfileExecutionGate
{
    /**
     * The authorization callback must perform a fresh fenced attempt heartbeat and throw on stop,
     * lease loss, revocation or binding mismatch. The native callback must stop its whole process
     * tree before returning or throwing. It may not expose the home to repository subprocesses.
     */
    public function execute(
        ProfileStore $store,
        array $binding,
        Closure $authorize,
        Closure $native,
    ): mixed {
        return $store->exclusively(function () use ($store, $binding, $authorize, $native): mixed {
            if ($store->read('execution') !== null) {
                throw new RuntimeException('Profile execution requires stopped recovery.');
            }

            if (($binding['profile_id'] ?? null) !== $store->profileId
                || ($binding['auth_mode'] ?? null) !== 'subscription'
                || $store->read('pending') !== null
                || NativeProfile::identity($store->read('active') ?? []) !== NativeProfile::identity($binding)) {
                throw new RuntimeException('Native profile is unavailable.');
            }
            $authorize();
            $store->validateHome();

            return $native($store->home(), $authorize);
        });
    }
}
