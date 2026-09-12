#!/usr/local/bin/node
// Synthetic Codex CLI boundary. Never loads account credentials or contacts a provider.
const fs = require('node:fs');
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
    emit({ type: 'thread.started', thread_id: 'synthetic-preflight-thread' });
    emit({ type: 'turn.started' });
    emit({ type: 'item.completed', item: { id: 'auth', type: 'agent_message', text: 'SHIPMUNK_AUTH_OK' } });
    emit({ type: 'turn.completed' });
} else if (args[0] === 'exec') {
    run().catch(() => {
        // Assertions may contain synthetic secrets. Keep the process error sanitized.
        process.stderr.write('Synthetic Codex integration boundary failed.\n');
        process.exit(1);
    });
} else {
    process.exit(2);
}

async function run() {
    assert.equal(process.env.HOME, '/profile');
    assert.equal(process.env.CODEX_HOME, '/profile/.codex');
    assert.equal(process.env.OPENAI_API_KEY, undefined);
    assert.equal(fs.readFileSync('/profile/synthetic-secret', 'utf8'), 'SYNTHETIC_PROFILE_SECRET');
    assert.equal(fs.existsSync('/workspace/README.md'), false);
    const configuration = args.filter(argument => argument.startsWith('developer_instructions='));
    assert.equal(configuration.length, 1);
    assert.ok(configuration[0].includes('APPROVED_BUNDLE'));
    assert.ok(configuration[0].includes('APPROVED_AGENT'));
    assert.equal(configuration[0].includes('UNTRUSTED_NATIVE_OVERRIDE'), false);
    assert.ok(args.includes('--strict-config'));
    assert.ok(args.includes('--ignore-user-config'));
    assert.ok(args.includes('--ignore-rules'));
    assert.ok(args.includes('forced_login_method="chatgpt"'));

    let context = '';
    for await (const chunk of process.stdin) context += chunk;
    assert.ok(context === 'SCENARIO:success' || context === 'SCENARIO:cancel');

    const bridge = spawn('/usr/local/bin/node', ['/usr/local/lib/shipmunk/codex-mcp.mjs'], {
        stdio: ['pipe', 'pipe', 'ignore'],
    });
    const responses = new Map();
    const lines = createInterface({ input: bridge.stdout });
    lines.on('line', line => {
        const response = JSON.parse(line);
        const waiting = responses.get(response.id);
        assert.ok(waiting);
        responses.delete(response.id);
        waiting.resolve(response);
    });
    bridge.on('exit', () => {
        for (const waiting of responses.values()) waiting.reject(new Error('Bridge stopped early.'));
        responses.clear();
    });
    let id = 0;
    const request = (method, params) => new Promise((resolve, reject) => {
        const current = ++id;
        responses.set(current, { resolve, reject });
        bridge.stdin.write(JSON.stringify({ jsonrpc: '2.0', id: current, method, params }) + '\n');
    });

    const initialized = await request('initialize', {
        protocolVersion: '2025-06-18',
        capabilities: {},
        clientInfo: { name: 'synthetic-supervisor-test', version: '1.0.0' },
    });
    assert.ok(initialized.result);
    bridge.stdin.write(JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' }) + '\n');
    const tools = await request('tools/list', {});
    assert.deepEqual(tools.result.tools.map(tool => tool.name), ['repository_command']);

    emit({ type: 'thread.started', thread_id: '0199a213-81c0-7800-8aa1-bbab2a035a53' });
    emit({ type: 'turn.started' });
    emit({ type: 'item.started', item: { id: 'repository', type: 'mcp_tool_call' } });
    const command = 'test ! -e /profile/synthetic-secret'
        + ' && test ! -e /bridge/request.json'
        + ' && test -z "$OPENAI_API_KEY"'
        + ' && test "$(cat README.md)" = original'
        + ' && test "$(cat AGENTS.md)" = UNTRUSTED_NATIVE_OVERRIDE'
        + ' && printf "changed\\n" > README.md'
        + ' && printf "added\\n" > added.txt'
        + ' && git add --all'
        + ' && git -c user.name=fixture -c user.email=fixture@example.test commit -qm hidden'
        + ' && (sleep 300 >/dev/null 2>&1 &)'
        + ' && printf REPOSITORY_BOUNDARY_OK';
    const tool = await request('tools/call', { name: 'repository_command', arguments: { command } });
    assert.equal(tool.result.isError, false);
    const output = JSON.parse(tool.result.content[0].text);
    assert.equal(output.stdout, 'REPOSITORY_BOUNDARY_OK');
    assert.equal(output.exit_code, 0);
    emit({ type: 'item.completed', item: { id: 'repository', type: 'mcp_tool_call' } });
    bridge.stdin.end();

    if (context === 'SCENARIO:cancel') {
        fs.writeFileSync('/profile/cancellation-ready', 'repository child is running', { mode: 0o600 });
        await new Promise(resolve => setTimeout(resolve, 30000));
        throw new Error('Supervisor cancellation did not stop the native process.');
    }

    emit({
        type: 'item.completed',
        item: {
            id: 'result',
            type: 'agent_message',
            text: JSON.stringify({
                summary: 'Synthetic repository changes verified.',
                outcome: 'changes_proposed',
                findings: [],
                tests: [{ command: 'synthetic repository boundary', status: 'passed', summary: 'Repository isolation checked.' }],
            }),
        },
    });
    emit({ type: 'turn.completed', usage: { input_tokens: 10, output_tokens: 20, cached_input_tokens: 0 } });
}
