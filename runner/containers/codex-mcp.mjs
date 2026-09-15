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

// exchangeBridge writes one fenceless request to the protected local bridge
// and waits for the trusted host supervisor's matching response. The request
// and response shapes are op-specific and validated by the caller: this
// function only enforces the generic file protocol (fresh files, size caps,
// matching sequence id).
async function exchangeBridge(fields) {
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
        await handle.writeFile(JSON.stringify({ id, ...fields }));
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
            value === null ||
            typeof value !== 'object' ||
            Array.isArray(value) ||
            value.id !== id
        ) {
            throw new Error('Invalid response.');
        }
        await unlink('/bridge/response.json');
        await unlink('/bridge/request.json');
        return value;
    }
    throw new Error('Bridge deadline exceeded.');
}

async function commandBridge(text) {
    const value = await exchangeBridge({ command: text });
    if (
        !object(value, ['id', 'stdout', 'stderr', 'exit_code']) ||
        typeof value.stdout !== 'string' ||
        typeof value.stderr !== 'string' ||
        !Number.isInteger(value.exit_code) ||
        value.exit_code < 0 ||
        value.exit_code > 255
    ) {
        throw new Error('Invalid response.');
    }
    return {
        content: [{ type: 'text', text: JSON.stringify(value) }],
        isError: value.exit_code !== 0,
    };
}

async function reviewBridge(op, args) {
    const value = await exchangeBridge({ op, ...args });
    if (
        !object(value, ['id', 'ok', 'output', 'truncated', 'snapshot_sha']) ||
        typeof value.ok !== 'boolean' ||
        typeof value.output !== 'string' ||
        typeof value.truncated !== 'boolean' ||
        (value.snapshot_sha !== undefined && typeof value.snapshot_sha !== 'string')
    ) {
        throw new Error('Invalid response.');
    }
    // snapshot_sha (review_list/review_read only) is the real 40-character
    // SHA of the snapshot the response came from; it must reach the model in
    // the text content, not just the outer mediator struct, or the model has
    // no way to cite an evidence snapshot that actually exists.
    const text = value.snapshot_sha
        ? `snapshot_sha: ${value.snapshot_sha}\n${value.output}`
        : value.output;
    return {
        content: [{ type: 'text', text }],
        isError: !value.ok,
    };
}

function validSnapshot(value) {
    return value === 'baseline' || value === 'workspace';
}

function validPath(value) {
    return (
        typeof value === 'string' &&
        Buffer.byteLength(value) <= 1024 &&
        !value.includes('\0')
    );
}

const reviewSnapshotSchema = { type: 'string', enum: ['baseline', 'workspace'] };
const reviewPathSchema = {
    type: 'string',
    maxLength: 1024,
    description: 'Path relative to the snapshot root. Use "" for the root.',
};

// The active tool set is chosen once from the fixed launch argument the
// runner supplies; it never changes for the life of this process, and a
// review process never advertises or accepts repository_command. An
// unrecognized argument fails closed rather than silently defaulting to the
// more permissive repository mode.
const modeArgument = process.argv[2];
if (modeArgument !== 'review' && modeArgument !== 'repository') {
    process.stderr.write('Unsupported repository bridge mode.\n');
    process.exit(1);
}
const mode = modeArgument;

const toolsByMode = {
    repository: [
        {
            name: 'repository_command',
            description:
                'Execute a command in the isolated task repository. Credentials and native configuration are unavailable there.',
            inputSchema: {
                type: 'object',
                properties: {
                    command: { type: 'string', minLength: 1, maxLength: 8192 },
                },
                required: ['command'],
                additionalProperties: false,
            },
            validate: (args) =>
                object(args, ['command']) &&
                typeof args.command === 'string' &&
                args.command.trim() !== '' &&
                !args.command.includes('\0') &&
                Buffer.byteLength(args.command) <= 8192,
            call: (args) => commandBridge(args.command),
        },
    ],
    review: [
        {
            name: 'review_list',
            description:
                'List the immediate entries of a directory in the authorized baseline or workspace snapshot. Pass the next offset from a truncated response to continue.',
            inputSchema: {
                type: 'object',
                properties: {
                    snapshot: reviewSnapshotSchema,
                    path: reviewPathSchema,
                    offset: {
                        type: 'integer',
                        minimum: 0,
                        description: 'Entries to skip before listing. Use 0 for the first page.',
                    },
                },
                required: ['snapshot', 'path', 'offset'],
                additionalProperties: false,
            },
            validate: (args) =>
                object(args, ['snapshot', 'path', 'offset']) &&
                validSnapshot(args.snapshot) &&
                validPath(args.path) &&
                Number.isSafeInteger(args.offset) &&
                args.offset >= 0,
            call: (args) =>
                reviewBridge('review_list', { snapshot: args.snapshot, path: args.path, offset: args.offset }),
        },
        {
            name: 'review_search',
            description:
                'Search file contents for a literal substring in the authorized baseline or workspace snapshot, optionally scoped to a path.',
            inputSchema: {
                type: 'object',
                properties: {
                    snapshot: reviewSnapshotSchema,
                    path: reviewPathSchema,
                    query: { type: 'string', minLength: 1, maxLength: 256 },
                },
                required: ['snapshot', 'path', 'query'],
                additionalProperties: false,
            },
            validate: (args) =>
                object(args, ['snapshot', 'path', 'query']) &&
                validSnapshot(args.snapshot) &&
                validPath(args.path) &&
                typeof args.query === 'string' &&
                args.query !== '' &&
                Buffer.byteLength(args.query) <= 256 &&
                !args.query.includes('\0'),
            call: (args) =>
                reviewBridge('review_search', {
                    snapshot: args.snapshot,
                    path: args.path,
                    query: args.query,
                }),
        },
        {
            name: 'review_read',
            description:
                'Read a bounded window of lines from one file in the authorized baseline or workspace snapshot.',
            inputSchema: {
                type: 'object',
                properties: {
                    snapshot: reviewSnapshotSchema,
                    path: reviewPathSchema,
                    start_line: { type: 'integer', minimum: 1 },
                    line_count: { type: 'integer', minimum: 1, maximum: 400 },
                },
                required: ['snapshot', 'path', 'start_line', 'line_count'],
                additionalProperties: false,
            },
            validate: (args) =>
                object(args, ['snapshot', 'path', 'start_line', 'line_count']) &&
                validSnapshot(args.snapshot) &&
                validPath(args.path) &&
                args.path !== '' &&
                Number.isSafeInteger(args.start_line) &&
                args.start_line >= 1 &&
                Number.isSafeInteger(args.line_count) &&
                args.line_count >= 1 &&
                args.line_count <= 400,
            call: (args) =>
                reviewBridge('review_read', {
                    snapshot: args.snapshot,
                    path: args.path,
                    start_line: args.start_line,
                    line_count: args.line_count,
                }),
        },
        {
            name: 'review_diff',
            description:
                'Return the changed-file list (path "") or a bounded unified diff for one path between the authorized baseline and workspace snapshots.',
            inputSchema: {
                type: 'object',
                properties: { path: reviewPathSchema },
                required: ['path'],
                additionalProperties: false,
            },
            validate: (args) => object(args, ['path']) && validPath(args.path),
            call: (args) => reviewBridge('review_diff', { path: args.path }),
        },
    ],
};

const activeTools = toolsByMode[mode];

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
            tools: activeTools.map(({ name, description, inputSchema }) => ({
                name,
                description,
                inputSchema,
            })),
        };
    } else if (message.method === 'tools/call') {
        const tool =
            object(params, ['name', 'arguments', '_meta']) &&
            metadata(params._meta) &&
            typeof params.name === 'string'
                ? activeTools.find((candidate) => candidate.name === params.name)
                : undefined;
        if (!tool || !tool.validate(params.arguments)) {
            return error(-32602, 'Invalid tool arguments.');
        }
        result = await tool.call(params.arguments);
        const envelope = { jsonrpc: '2.0', id, result };
        if (Buffer.byteLength(JSON.stringify(envelope) + '\n') > outputLimit) {
            result = {
                content: [
                    {
                        type: 'text',
                        // Repository mode keeps its exact original wording, a
                        // fixture string the PHP boundary test still checks.
                        text:
                            mode === 'repository'
                                ? 'Repository command finished, but its response exceeds the MCP output limit. Inspect results with a command that produces less output.'
                                : 'Tool call finished, but its response exceeds the MCP output limit. Request a narrower result.',
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
