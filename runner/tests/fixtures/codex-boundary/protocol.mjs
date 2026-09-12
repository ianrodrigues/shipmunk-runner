import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createInterface } from 'node:readline';
import {
    readFile,
    writeFile,
    readdir,
    rm,
    symlink,
    rename,
} from 'node:fs/promises';
import { setTimeout as delay } from 'node:timers/promises';

async function clean() {
    for (const file of await readdir('/bridge'))
        await rm('/bridge/' + file, { force: true });
}

function session() {
    const child = spawn('/usr/local/bin/node', ['/fixture/codex-mcp.mjs'], {
        env: { PATH: '/usr/local/bin:/usr/bin:/bin' },
        stdio: ['pipe', 'pipe', 'pipe'],
    });
    const messages = [];
    createInterface({ input: child.stdout }).on('line', (line) =>
        messages.push(JSON.parse(line)),
    );
    let stderr = '';
    child.stderr.on('data', (bytes) => (stderr += bytes));
    const exited = new Promise((resolve) =>
        child.once('exit', (code) => resolve({ code, stderr })),
    );
    return {
        child,
        messages,
        exited,
        send: (message) => child.stdin.write(JSON.stringify(message) + '\n'),
        async receive(id) {
            for (let attempt = 0; attempt < 200; attempt++) {
                const message = messages.find((message) => message.id === id);
                if (message) return message;
                await delay(10);
            }
            throw new Error('MCP response was not received.');
        },
        async close() {
            child.stdin.end();
            assert.equal((await exited).code, 0);
        },
    };
}

function request(id, method, params = {}) {
    return { jsonrpc: '2.0', id, method, params };
}
async function initialize(client, version = '2025-06-18') {
    client.send(
        request(1, 'initialize', {
            protocolVersion: version,
            capabilities: {},
            clientInfo: { name: 'offline-fixture', version: '1' },
        }),
    );
    assert.equal((await client.receive(1)).result.protocolVersion, version);
    client.send({ jsonrpc: '2.0', method: 'notifications/initialized' });
}
async function spool() {
    for (let attempt = 0; attempt < 200; attempt++) {
        try {
            return JSON.parse(await readFile('/bridge/request.json', 'utf8'));
        } catch (error) {
            if (error.code !== 'ENOENT') throw error;
            await delay(10);
        }
    }
    throw new Error('Bridge request was not published.');
}
async function respond(value) {
    await writeFile('/bridge/response.tmp', JSON.stringify(value));
    await rename('/bridge/response.tmp', '/bridge/response.json');
}

for (const version of ['2024-11-05', '2025-03-26', '2025-06-18']) {
    const client = session();
    await initialize(client, version);
    client.send(request(2, 'ping'));
    assert.deepEqual((await client.receive(2)).result, {});
    client.send(request(3, 'tools/list'));
    assert.deepEqual(
        (await client.receive(3)).result.tools.map((tool) => tool.name),
        ['repository_command'],
    );
    await client.close();
}
console.log(
    'PASS MCP protocol initialization, ping and single repository tool discovery',
);

const uninitialized = session();
uninitialized.send(
    request(1, 'tools/call', {
        name: 'repository_command',
        arguments: { command: 'id' },
    }),
);
assert.equal((await uninitialized.receive(1)).error.code, -32002);
assert.deepEqual(await readdir('/bridge'), []);
await uninitialized.close();

const invalid = session();
await initialize(invalid);
invalid.send(
    request(8, 'initialize', {
        protocolVersion: '2025-06-18',
        capabilities: {},
        clientInfo: { name: 'offline-fixture', version: '1' },
    }),
);
assert.equal((await invalid.receive(8)).error.code, -32602);
assert.deepEqual(await readdir('/bridge'), []);
const cases = [
    request(2, 'resources/read', { uri: 'file:///profile/.codex/auth.canary' }),
    request(3, 'tools/call', { name: 'shell', arguments: { command: 'id' } }),
    request(4, 'tools/call', {
        name: 'repository_command',
        arguments: { command: 'id', cwd: '/profile' },
    }),
    request(5, 'tools/call', {
        name: 'repository_command',
        arguments: { command: 'x'.repeat(8193) },
    }),
    request(6, 'tools/call', {
        name: 'repository_command',
        arguments: { command: '\0' },
    }),
    request(7, 'tools/call', { name: 'repository_command', arguments: {} }),
];
for (const message of cases) {
    invalid.send(message);
    assert.ok((await invalid.receive(message.id)).error);
}
assert.deepEqual(await readdir('/bridge'), []);
await invalid.close();
console.log(
    'PASS unknown tools, methods and invalid command arguments cause no filesystem effects',
);

const serial = session();
await initialize(serial);
serial.send(
    request(2, 'tools/call', {
        _meta: {
            callId: 'fixture',
            threadId: 'thread',
            itemId: 'item',
            progressToken: 1,
            'x-codex-turn-metadata': { ignored: true },
        },
        name: 'repository_command',
        arguments: { command: 'touch /profile/evil' },
    }),
);
serial.send(
    request(3, 'tools/call', {
        name: 'repository_command',
        arguments: { command: 'second' },
    }),
);
assert.deepEqual(await spool(), { id: 1, command: 'touch /profile/evil' });
await delay(50);
assert.deepEqual(await spool(), { id: 1, command: 'touch /profile/evil' });
await respond({ id: 1, stdout: 'first', stderr: '', exit_code: 0 });
assert.equal((await serial.receive(2)).result.isError, false);
assert.deepEqual(await spool(), { id: 2, command: 'second' });
await respond({ id: 2, stdout: '', stderr: 'failed', exit_code: 2 });
assert.equal((await serial.receive(3)).result.isError, true);
assert.equal((await readdir('/profile')).includes('evil'), false);
assert.deepEqual(await readdir('/bridge'), []);
await serial.close();
console.log(
    'PASS concurrent MCP calls serialize atomic request IDs and execute no local commands',
);

for (const escaped of [false, true]) {
    const client = session();
    await initialize(client);
    client.send(
        request(2, 'tools/call', {
            name: 'repository_command',
            arguments: { command: 'large-output' },
        }),
    );
    await spool();
    const response = { id: 1, stdout: '', stderr: '', exit_code: 0 };
    const remaining = 65536 - Buffer.byteLength(JSON.stringify(response));
    response.stdout = escaped
        ? '\\'.repeat(Math.floor(remaining / 2))
        : 'x'.repeat(remaining);
    assert.ok(Buffer.byteLength(JSON.stringify(response)) <= 65536);
    const started = performance.now();
    await respond(response);
    if (escaped) {
        const result = (await client.receive(2)).result;
        assert.equal(result.isError, true);
        assert.equal(
            result.content[0].text,
            'Repository command finished, but its response exceeds the MCP output limit. Inspect results with a command that produces less output.',
        );
        assert.ok(performance.now() - started < 10000);
        assert.deepEqual(await readdir('/bridge'), []);
        client.send(
            request(3, 'tools/call', {
                name: 'repository_command',
                arguments: { command: 'retry-with-bounded-output' },
            }),
        );
        assert.deepEqual(await spool(), {
            id: 2,
            command: 'retry-with-bounded-output',
        });
        await respond({ id: 2, stdout: 'recovered', stderr: '', exit_code: 0 });
        assert.equal((await client.receive(3)).result.isError, false);
        await client.close();
    } else {
        assert.deepEqual(
            JSON.parse((await client.receive(2)).result.content[0].text),
            response,
        );
        await client.close();
    }
    await clean();
}
console.log(
    'PASS maximum host responses return a bounded recoverable error when their MCP envelope exceeds 128 KiB',
);

for (const response of [
    { id: 99, stdout: '', stderr: '', exit_code: 0 },
    { id: 1, stdout: '', stderr: '', exit_code: 0, credential: 'forbidden' },
    { id: 1, stdout: 'x'.repeat(65537), stderr: '', exit_code: 0 },
    { id: 1, stdout: '', stderr: '', exit_code: -1 },
    'symlink',
]) {
    const client = session();
    await initialize(client);
    client.send(
        request(2, 'tools/call', {
            name: 'repository_command',
            arguments: { command: 'id' },
        }),
    );
    await spool();
    const rejectionStarted = performance.now();
    if (response === 'symlink')
        await symlink('/profile/.codex/auth.canary', '/bridge/response.json');
    else await respond(response);
    assert.equal((await client.exited).code, 1);
    assert.ok(
        performance.now() - rejectionStarted < 10000,
        'Malformed response was handled by the command deadline instead of immediate validation.',
    );
    assert.equal(
        client.messages.some((message) => message.id === 2),
        false,
    );
    await clean();
}
console.log(
    'PASS stale, malformed, oversized and symlink host responses terminate the bridge',
);

for (const bytes of ['x'.repeat(65537), '{"jsonrpc":"2.0"}\n', '{']) {
    const client = session();
    client.child.stdin.end(bytes);
    assert.equal((await client.exited).code, 1);
}
console.log('PASS oversized, malformed and truncated MCP input fails closed');

const timed = session();
await initialize(timed);
timed.send(
    request(2, 'tools/call', {
        name: 'repository_command',
        arguments: { command: 'unanswered' },
    }),
);
await spool();
const started = performance.now();
assert.equal((await timed.exited).code, 1);
assert.ok(performance.now() - started >= 29000);
assert.equal(
    timed.messages.some((message) => message.id === 2),
    false,
);
await clean();
console.log(
    'PASS unanswered repository command terminates the bridge after its bounded deadline',
);
