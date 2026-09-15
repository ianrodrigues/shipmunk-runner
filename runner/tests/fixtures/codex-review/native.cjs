#!/usr/local/bin/node
// Synthetic Codex CLI for the review boundary. The fixture never loads account
// credentials or contacts a provider. The fixture only proves the real
// Docker/MCP path end to end.
const { spawn } = require('node:child_process');
const { createInterface } = require('node:readline');
const assert = require('node:assert/strict');

const args = process.argv.slice(2);
const emit = value => process.stdout.write(JSON.stringify(value) + '\n');

if (args.length === 1 && args[0] === '--version') {
    console.log('codex-cli 0.154.0');
} else if (args.join(' ') === 'login status') {
    console.log('Logged in using ChatGPT');
} else if (args[0] === 'exec' && args.includes('--ephemeral')) {
    emit({ type: 'thread.started', thread_id: 'synthetic-review-preflight-thread' });
    emit({ type: 'turn.started' });
    emit({ type: 'item.completed', item: { id: 'auth', type: 'agent_message', text: 'SHIPMUNK_AUTH_OK' } });
    emit({ type: 'turn.completed' });
} else if (args[0] === 'exec') {
    run().catch(error => {
        process.stderr.write('Synthetic review boundary failed: ' + (error && error.message) + '\n');
        process.exit(1);
    });
} else {
    process.exit(2);
}

async function run() {
    const configuration = args.filter(argument => argument.startsWith('developer_instructions='));
    assert.equal(configuration.length, 1);
    const developer = JSON.parse(configuration[0].slice('developer_instructions='.length));
    assert.ok(developer.includes('review_list'));
    assert.ok(developer.includes('charter_version'));
    const baselineMatch = developer.match(/Cite the baseline snapshot as "([a-f0-9]{40})"/);
    const headMatch = developer.match(/workspace \(head\) snapshot as "([a-f0-9]{40})"/);
    assert.ok(baselineMatch && headMatch, 'prompt did not carry the real snapshot SHAs');

    let stdin = '';
    for await (const chunk of process.stdin) stdin += chunk;
    assert.ok(stdin.length > 0);

    const bridge = spawn('/usr/local/bin/node', ['/usr/local/lib/shipmunk/codex-mcp.mjs', 'review'], {
        stdio: ['pipe', 'pipe', 'ignore'],
    });
    const pending = new Map();
    createInterface({ input: bridge.stdout }).on('line', line => {
        const response = JSON.parse(line);
        const waiting = pending.get(response.id);
        if (!waiting) return;
        pending.delete(response.id);
        waiting.resolve(response);
    });
    bridge.on('exit', () => {
        for (const waiting of pending.values()) waiting.reject(new Error('Review bridge stopped early.'));
        pending.clear();
    });
    let nextID = 0;
    const request = (method, params) => new Promise((resolve, reject) => {
        const id = ++nextID;
        pending.set(id, { resolve, reject });
        bridge.stdin.write(JSON.stringify({ jsonrpc: '2.0', id, method, params }) + '\n');
    });
    const call = async (name, argv) => {
        const response = await request('tools/call', { name, arguments: argv });
        assert.equal(response.result.isError, false, `${name} failed: ${JSON.stringify(response.result)}`);
        return response.result.content[0].text;
    };

    await request('initialize', {
        protocolVersion: '2025-06-18',
        capabilities: {},
        clientInfo: { name: 'synthetic-review-test', version: '1.0.0' },
    });
    bridge.stdin.write(JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' }) + '\n');
    const tools = await request('tools/list', {});
    assert.deepEqual(
        tools.result.tools.map(tool => tool.name).sort(),
        ['review_diff', 'review_list', 'review_read', 'review_search'],
    );

    emit({ type: 'thread.started', thread_id: '0199a213-81c0-7800-8aa1-bbab2a035a53' });
    emit({ type: 'turn.started' });
    emit({ type: 'item.started', item: { id: 'diff', type: 'mcp_tool_call' } });
    const changedListing = await call('review_diff', { path: '' });
    emit({ type: 'item.completed', item: { id: 'diff', type: 'mcp_tool_call' } });

    const files = changedListing === 'no changes'
        ? []
        : changedListing.split('\n').map(line => line.split('\t')[1]);

    emit({ type: 'item.started', item: { id: 'list', type: 'mcp_tool_call' } });
    await call('review_list', { snapshot: 'workspace', path: '', offset: 0 });
    emit({ type: 'item.completed', item: { id: 'list', type: 'mcp_tool_call' } });

    bridge.stdin.end();

    emit({
        type: 'item.completed',
        item: {
            id: 'result',
            type: 'agent_message',
            text: JSON.stringify({
                summary: 'Declared and observed behaviour agree; nothing reachable was found in the changed files.',
                outcome: 'no_findings',
                charter_version: '1',
                findings: [],
                coverage: {
                    files: files.map(path => ({ path, status: 'reviewed' })),
                    context_gaps: [],
                },
                verification_state: 'none',
                tests: [],
            }),
        },
    });
    emit({ type: 'turn.completed', usage: { input_tokens: 12, output_tokens: 24, cached_input_tokens: 0 } });
}
