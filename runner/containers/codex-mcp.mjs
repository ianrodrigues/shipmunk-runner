import { constants } from 'node:fs';
import { open, rename, lstat, unlink } from 'node:fs/promises';
import { once } from 'node:events';
import { performance } from 'node:perf_hooks';
import { setTimeout as delay } from 'node:timers/promises';

const lineLimit = 65536;
const outputLimit = 131072;
const responseLimit = 65536;
const versions = ['2024-11-05', '2025-03-26', '2025-06-18'];
let initialized = false;
let sequence = 0;

function object(value, keys) {
    return (
        value !== null &&
        typeof value === 'object' &&
        !Array.isArray(value) &&
        Object.keys(value).every((key) => keys.includes(key))
    );
}

function metadata(value) {
    if (value === undefined) {
        return true;
    }
    if (
        !object(value, [
            'progressToken',
            'callId',
            'threadId',
            'itemId',
            'x-codex-turn-metadata',
        ])
    ) {
        return false;
    }
    if (
        value.progressToken !== undefined &&
        typeof value.progressToken !== 'string' &&
        !Number.isSafeInteger(value.progressToken)
    ) {
        return false;
    }
    for (const key of ['callId', 'threadId', 'itemId']) {
        if (
            value[key] !== undefined &&
            (typeof value[key] !== 'string' || value[key].length > 128)
        ) {
            return false;
        }
    }
    // Pinned Codex adds opaque trace metadata. It is never forwarded to the repository spool.
    const trace = value['x-codex-turn-metadata'];
    return (
        trace === undefined ||
        (trace !== null && typeof trace === 'object' && !Array.isArray(trace))
    );
}

function fatal() {
    process.stderr.write('Repository bridge failed.\n');
    process.exit(1);
}

async function emit(value) {
    const bytes = JSON.stringify(value) + '\n';
    if (Buffer.byteLength(bytes) > outputLimit) {
        fatal();
    }
    if (!process.stdout.write(bytes)) {
        await once(process.stdout, 'drain');
    }
}

async function command(command) {
    const id = ++sequence;
    for (const path of ['/bridge/request.json', '/bridge/response.json']) {
        try {
            await lstat(path);
            throw new Error('Stale bridge file.');
        } catch (error) {
            if (error.code !== 'ENOENT') {
                throw error;
            }
        }
    }
    const handle = await open(
        '/bridge/request.tmp',
        constants.O_WRONLY |
            constants.O_CREAT |
            constants.O_EXCL |
            constants.O_NOFOLLOW,
        0o600,
    );
    try {
        await handle.writeFile(JSON.stringify({ id, command }));
        await handle.sync();
    } finally {
        await handle.close();
    }
    await rename('/bridge/request.tmp', '/bridge/request.json');
    const deadline = performance.now() + 30000;
    while (performance.now() < deadline) {
        let response;
        try {
            response = await open(
                '/bridge/response.json',
                constants.O_RDONLY |
                    constants.O_NOFOLLOW |
                    constants.O_NONBLOCK,
            );
        } catch (error) {
            if (error.code !== 'ENOENT') {
                throw error;
            }
            await delay(25);
            continue;
        }
        let value;
        try {
            const stat = await response.stat();
            if (!stat.isFile() || stat.size > responseLimit) {
                throw new Error('Invalid response.');
            }
            const bytes = Buffer.alloc(responseLimit + 1);
            const { bytesRead } = await response.read(
                bytes,
                0,
                bytes.length,
                0,
            );
            if (bytesRead > responseLimit || bytesRead !== stat.size) {
                throw new Error('Invalid response.');
            }
            value = JSON.parse(bytes.subarray(0, bytesRead).toString('utf8'));
        } finally {
            await response.close();
        }
        if (
            !object(value, ['id', 'stdout', 'stderr', 'exit_code']) ||
            value.id !== id ||
            typeof value.stdout !== 'string' ||
            typeof value.stderr !== 'string' ||
            !Number.isInteger(value.exit_code) ||
            value.exit_code < 0 ||
            value.exit_code > 255
        ) {
            throw new Error('Invalid response.');
        }
        await unlink('/bridge/response.json');
        await unlink('/bridge/request.json');
        return {
            content: [{ type: 'text', text: JSON.stringify(value) }],
            isError: value.exit_code !== 0,
        };
    }
    throw new Error('Bridge deadline exceeded.');
}

async function dispatch(message) {
    if (
        !object(message, ['jsonrpc', 'id', 'method', 'params']) ||
        message.jsonrpc !== '2.0' ||
        typeof message.method !== 'string'
    ) {
        fatal();
    }
    const id = message.id;
    if (id === undefined) {
        if (
            message.method === 'notifications/initialized' &&
            initialized &&
            (message.params === undefined || object(message.params, []))
        ) {
            return;
        }
        fatal();
    }
    if (
        !(typeof id === 'string' && id.length <= 128) &&
        !(Number.isSafeInteger(id) && id >= 0)
    ) {
        fatal();
    }
    const error = (code, text) =>
        emit({ jsonrpc: '2.0', id, error: { code, message: text } });
    const params = message.params === undefined ? {} : message.params;
    let result;
    if (message.method === 'initialize') {
        if (
            initialized ||
            !object(params, [
                'protocolVersion',
                'capabilities',
                'clientInfo',
            ]) ||
            !versions.includes(params.protocolVersion) ||
            !object(
                params.capabilities,
                Object.keys(params.capabilities ?? {}),
            ) ||
            !object(params.clientInfo, [
                'name',
                'version',
                'title',
                'description',
                'websiteUrl',
                'icons',
            ]) ||
            typeof params.clientInfo.name !== 'string' ||
            typeof params.clientInfo.version !== 'string'
        ) {
            return error(-32602, 'Invalid initialization.');
        }
        initialized = true;
        result = {
            protocolVersion: params.protocolVersion,
            capabilities: { tools: {} },
            serverInfo: { name: 'shipmunk-repository', version: '1.0.0' },
        };
    } else if (!initialized) {
        return error(-32002, 'Initialization required.');
    } else if (
        message.method === 'ping' &&
        object(params, ['_meta']) &&
        metadata(params._meta)
    ) {
        result = {};
    } else if (
        message.method === 'tools/list' &&
        object(params, ['_meta']) &&
        metadata(params._meta)
    ) {
        result = {
            tools: [
                {
                    name: 'repository_command',
                    description:
                        'Execute a command in the isolated task repository. Credentials and native configuration are unavailable there.',
                    inputSchema: {
                        type: 'object',
                        properties: {
                            command: {
                                type: 'string',
                                minLength: 1,
                                maxLength: 8192,
                            },
                        },
                        required: ['command'],
                        additionalProperties: false,
                    },
                },
            ],
        };
    } else if (message.method === 'tools/call') {
        if (
            !object(params, ['name', 'arguments', '_meta']) ||
            params.name !== 'repository_command' ||
            !object(params.arguments, ['command']) ||
            typeof params.arguments.command !== 'string' ||
            params.arguments.command.trim() === '' ||
            params.arguments.command.includes('\0') ||
            Buffer.byteLength(params.arguments.command) > 8192 ||
            !metadata(params._meta)
        ) {
            return error(-32602, 'Invalid repository command.');
        }
        result = await command(params.arguments.command);
        const envelope = { jsonrpc: '2.0', id, result };
        if (Buffer.byteLength(JSON.stringify(envelope) + '\n') > outputLimit) {
            result = {
                content: [
                    {
                        type: 'text',
                        text: 'Repository command finished, but its response exceeds the MCP output limit. Inspect results with a command that produces less output.',
                    },
                ],
                isError: true,
            };
        }
    } else {
        return error(-32601, 'Unsupported method.');
    }
    await emit({ jsonrpc: '2.0', id, result });
}

let pending = Buffer.alloc(0);
try {
    for await (const chunk of process.stdin) {
        pending = Buffer.concat([pending, chunk]);
        while (true) {
            const newline = pending.indexOf(10);
            if (newline < 0) {
                break;
            }
            if (newline >= lineLimit) {
                fatal();
            }
            const line = pending.subarray(0, newline);
            pending = pending.subarray(newline + 1);
            await dispatch(JSON.parse(line.toString('utf8')));
        }
        if (pending.length > lineLimit) {
            fatal();
        }
    }
    if (pending.length !== 0) {
        fatal();
    }
} catch {
    fatal();
}
