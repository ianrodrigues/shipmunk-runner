// Run only in a disposable network-none container. No existing repository or home is used.
import { mkdirSync, writeFileSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { gzipSync } from 'node:zlib';

const cwd = '/tmp/source-archive-fixture';
mkdirSync(cwd);
mkdirSync(cwd + '/src');
writeFileSync(cwd + '/src/index.txt', 'fixture\n');
writeFileSync(cwd + '/run.sh', '#!/bin/sh\nprintf fixture\\n\n', {
    mode: 0o755,
});
const env = {
    PATH: '/usr/bin:/bin',
    HOME: '/tmp',
    GIT_CONFIG_NOSYSTEM: '1',
    GIT_CONFIG_GLOBAL: '/dev/null',
    GIT_AUTHOR_NAME: 'Offline Fixture',
    GIT_AUTHOR_EMAIL: 'fixture@example.invalid',
    GIT_COMMITTER_NAME: 'Offline Fixture',
    GIT_COMMITTER_EMAIL: 'fixture@example.invalid',
    GIT_AUTHOR_DATE: '2026-01-01T00:00:00Z',
    GIT_COMMITTER_DATE: '2026-01-01T00:00:00Z',
};
const git = (...args) =>
    execFileSync('git', args, { cwd, env, stdio: ['ignore', 'pipe', 'pipe'] });
git('init');
git('add', '.');
git('commit', '-m', 'Synthetic archive fixture');
const base = git('rev-parse', 'HEAD').toString().trim();
const archive = (...args) =>
    gzipSync(git('archive', '--format=tar', ...args)).toString('base64');
const fixtures = {
    git_version: git('--version').toString().trim(),
    base_sha: base,
    short: archive('--prefix=owner-demo-' + base.slice(0, 7) + '/', 'HEAD'),
    full: archive('--prefix=owner-demo-' + base + '/', 'HEAD'),
    executable: archive('HEAD^{tree}', 'run.sh'),
    subtree: archive('HEAD^{tree}', 'src'),
};
writeFileSync(cwd + '/' + 'x'.repeat(120), 'long path\n');
git('add', '.');
git('commit', '-m', 'Synthetic long path');
fixtures.head_sha = git('rev-parse', 'HEAD').toString().trim();
fixtures.long = archive(
    '--prefix=owner-demo-' + fixtures.head_sha.slice(0, 7) + '/',
    'HEAD',
);
console.log(JSON.stringify(fixtures, null, 2));
