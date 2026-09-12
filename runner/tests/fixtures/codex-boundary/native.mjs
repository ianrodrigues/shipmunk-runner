// Offline transport fixture for official codex-cli 0.154.0. Never use for account authentication.
import assert from 'node:assert/strict';
import { spawn, execFileSync } from 'node:child_process';
import { createServer } from 'node:http';
import {
    mkdir,
    readFile,
    writeFile,
    readdir,
    rm,
    rename,
} from 'node:fs/promises';
import { setTimeout as delay } from 'node:timers/promises';

const home = '/profile/.codex';
const canary = 'forced_login_method = "chatgpt"\n';
const environment = {
    PATH: '/usr/local/bin:/usr/bin:/bin',
    HOME: '/profile',
    CODEX_HOME: home,
    LANG: 'C.UTF-8',
};
await mkdir(home, { recursive: true });
assert.equal(
    execFileSync('codex', ['--version'], { env: environment })
        .toString()
        .trim(),
    'codex-cli 0.154.0',
);
await mkdir('/tmp/work', { recursive: true });
await writeFile(home + '/config.toml', canary);
await writeFile(home + '/auth.canary', 'SYNTHETIC_CREDENTIAL_CANARY');

// Capture the production driver's real arguments in PHP; only the loopback provider is substituted.
const productionArguments = JSON.parse(process.env.SHIPMUNK_FIXTURE_ARGV);
assert.equal(productionArguments[0], '/usr/local/bin/codex');
assert.equal(productionArguments[1], 'exec');
assert.equal(productionArguments.at(-1), '-');
const secureArguments = productionArguments.slice(2, -1);

function processResult(command, args, input = '') {
    const process = spawn(command, args, {
        env: environment,
        detached: true,
        stdio: ['pipe', 'pipe', 'pipe'],
    });
    let stdout = '',
        stderr = '';
    process.stdout.on('data', (chunk) => {
        stdout += chunk;
        if (stdout.length > 1048576) process.kill('SIGKILL');
    });
    process.stderr.on('data', (chunk) => {
        stderr += chunk;
        if (stderr.length > 1048576) process.kill('SIGKILL');
    });
    process.stdin.end(input);
    return new Promise((resolve, reject) => {
        const timer = setTimeout(() => {
            process.kill('SIGKILL');
            reject(new Error('Native fixture timed out.'));
        }, 20000);
        process.once('error', reject);
        process.once('exit', (code, signal) => {
            clearTimeout(timer);
            try {
                globalThis.process.kill(-process.pid, 'SIGKILL');
            } catch {}
            resolve({ code, signal, stdout, stderr });
        });
    });
}

async function runNative(
    item,
    extra = [],
    mediate = false,
    rejection = null,
    model = 'gpt-5.4',
) {
    const requests = [];
    const directSearch = mediate === true;
    let failure;
    const server = createServer(async (request, response) => {
        try {
            assert.equal(request.url, '/v1/responses');
            const chunks = [];
            for await (const chunk of request) chunks.push(chunk);
            const body = JSON.parse(Buffer.concat(chunks));
            requests.push(body);

            const schema = body.text.format.schema;
            assert.equal(schema.properties.outcome.type, 'string');
            assert.equal(
                schema.properties.findings.items.properties.side.type,
                'string',
            );
            assert.equal(
                schema.properties.findings.items.properties.severity.type,
                'string',
            );
            assert.equal(
                schema.properties.tests.items.properties.status.type,
                'string',
            );
            assert.equal(
                Object.hasOwn(
                    schema.properties.findings.items.properties.path,
                    'pattern',
                ),
                false,
            );

            if (rejection !== null) {
                response.writeHead(400, { 'Content-Type': 'application/json' });
                response.end(JSON.stringify({ error: rejection }));
                return;
            }

            response.writeHead(200, { 'Content-Type': 'text/event-stream' });
            const send = (data) =>
                response.write('data: ' + JSON.stringify(data) + '\n\n');
            const output =
                requests.length === 1 && directSearch
                    ? {
                          type: 'tool_search_call',
                          id: 'search_fixture',
                          call_id: 'search_fixture',
                          execution: 'client',
                          arguments: { query: 'repository_command', limit: 1 },
                      }
                    : requests.length === (directSearch ? 2 : 1)
                      ? item
                      : {
                            type: 'message',
                            role: 'assistant',
                            id: 'msg_fixture',
                            status: 'completed',
                            content: [
                                {
                                    type: 'output_text',
                                    text: JSON.stringify({
                                        summary: 'Offline fixture complete.',
                                        outcome: 'no_findings',
                                        findings: [],
                                        tests: [],
                                    }),
                                    annotations: [],
                                },
                            ],
                        };
            send({
                type: 'response.created',
                response: {
                    id: 'resp_fixture',
                    status: 'in_progress',
                    output: [],
                },
            });
            send({
                type: 'response.output_item.added',
                output_index: 0,
                item: output,
            });
            send({
                type: 'response.output_item.done',
                output_index: 0,
                item: output,
            });
            send({
                type: 'response.completed',
                response: {
                    id: 'resp_fixture',
                    status: 'completed',
                    output: [output],
                    usage: {
                        input_tokens: 1,
                        output_tokens: 1,
                        total_tokens: 2,
                    },
                },
            });
            response.end();
        } catch (error) {
            failure = error;
            response.writeHead(500);
            response.end();
        }
    });
    await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
    const args = [
        'exec',
        ...secureArguments.map((argument, index) =>
            secureArguments[index - 1] === '--model' ? model : argument,
        ),
        '-c',
        'model_provider="fixture"',
        '-c',
        `model_providers.fixture={name="offline fixture",base_url="http://127.0.0.1:${server.address().port}/v1",wire_api="responses",requires_openai_auth=false,request_max_retries=0,stream_max_retries=0}`,
        ...extra,
        '-',
    ];
    let mediated;
    let nativeFinished = false;
    const monitor = (
        mediate
            ? (async () => {
                  for (let attempt = 0; attempt < 600; attempt++) {
                      try {
                          mediated = JSON.parse(
                              await readFile('/bridge/request.json', 'utf8'),
                          );
                          assert.deepEqual(mediated, {
                              id: 1,
                              command: 'touch /profile/evil',
                          });
                          await writeFile(
                              '/bridge/response.tmp',
                              JSON.stringify({
                                  id: 1,
                                  stdout: 'HOST_FIXTURE_ONLY',
                                  stderr: '',
                                  exit_code: 0,
                              }),
                          );
                          await rename(
                              '/bridge/response.tmp',
                              '/bridge/response.json',
                          );
                          return;
                      } catch (error) {
                          if (error.code !== 'ENOENT') throw error;
                          if (nativeFinished)
                              throw new Error(
                                  'Native runtime completed without the bridge call.',
                              );
                          await delay(25);
                      }
                  }
                  throw new Error('Native MCP call never reached bridge.');
              })()
            : Promise.resolve()
    ).then(
        () => null,
        (error) => error,
    );
    try {
        const result = await processResult(
            'codex',
            args,
            'Offline boundary fixture.',
        );
        nativeFinished = true;
        const monitorError = await monitor;
        if (monitorError) {
            throw new Error(
                monitorError.message +
                    '\n' +
                    result.stderr +
                    '\n' +
                    result.stdout +
                    '\nTools: ' +
                    JSON.stringify(
                        requests.map((request) => ({
                            tools: request.tools,
                            outputs: request.input.filter((item) =>
                                item.type.includes('tool'),
                            ),
                        })),
                    ),
            );
        }
        if (failure) throw failure;
        return { ...result, requests, mediated };
    } finally {
        server.closeAllConnections();
        await new Promise((resolve) => server.close(resolve));
    }
}

const patches = {
    bridge_spoof:
        '*** Add File: /bridge/response.json\n+{"id":1,"stdout":"spoof","stderr":"","exit_code":0}\n',
    add: '*** Add File: /profile/.codex/evil.toml\n+synthetic=true\n',
    update: '*** Update File: /profile/.codex/config.toml\n@@\n-forced_login_method = "chatgpt"\n+forced_login_method = "api"\n',
    delete: '*** Delete File: /profile/.codex/auth.canary\n',
};
for (const [name, patch] of Object.entries(patches)) {
    const result = await runNative({
        type: 'custom_tool_call',
        id: 'fc_fixture',
        call_id: 'call_fixture',
        name: 'apply_patch',
        input: '*** Begin Patch\n' + patch + '*** End Patch',
    });
    assert.equal(result.code, 0, result.stderr);
    assert.deepEqual(
        result.requests[0].tools.map((tool) => tool.name ?? tool.type).sort(),
        [
            'apply_patch',
            'list_mcp_resource_templates',
            'list_mcp_resources',
            'read_mcp_resource',
            'request_user_input',
            'tool_search',
        ],
    );
    const output = result.requests[1].input.find(
        (item) => item.type === 'custom_tool_call_output',
    );
    assert.match(
        output.output,
        /patch rejected|apply_patch verification failed/,
    );
    assert.equal(await readFile(home + '/config.toml', 'utf8'), canary);
    assert.equal(
        await readFile(home + '/auth.canary', 'utf8'),
        'SYNTHETIC_CREDENTIAL_CANARY',
    );
    assert.equal((await readdir(home)).includes('evil.toml'), false);
    assert.deepEqual(await readdir('/bridge'), []);
    console.log(
        `PASS pinned native local apply_patch ${name} cannot modify protected profile`,
    );
}

const invalid = await processResult('codex', [
    'exec',
    ...secureArguments,
    '-c',
    'tools.apply_patch=false',
    'Offline fixture.',
]);
assert.notEqual(invalid.code, 0);
assert.match(invalid.stderr, /unknown configuration field `tools.apply_patch`/);
console.log('PASS unsupported local-tool disable configuration is rejected');

const mcp = await runNative(
    {
        type: 'function_call',
        id: 'fc_fixture',
        call_id: 'call_fixture',
        namespace: 'mcp__repository',
        name: 'repository_command',
        arguments: JSON.stringify({ command: 'touch /profile/evil' }),
    },
    [],
    true,
);
assert.equal(mcp.code, 0, mcp.stderr);
assert.ok(mcp.requests[0].tools.some((tool) => tool.type === 'tool_search'));
assert.deepEqual(mcp.mediated, { id: 1, command: 'touch /profile/evil' });
assert.equal((await readdir('/profile')).includes('evil'), false);
assert.deepEqual(await readdir('/bridge'), []);
assert.ok(
    JSON.stringify(mcp.requests.at(-1).input).includes('HOST_FIXTURE_ONLY'),
);
console.log(
    'PASS pinned native MCP initialization and command use only the trusted host bridge',
);

// Pass the actual CLI wire output to the host parser; these errors are synthetic.
for (const code of ['model_not_found', 'invalid_json_schema', 'unknown']) {
    const result = await runNative(null, [], false, {
        code,
        type: 'invalid_request_error',
        message: 'SYNTHETIC_SECRET_BACKEND_REJECTION',
    });
    assert.equal(result.code, 1);
    assert.equal(result.requests.length, 1);
    console.log(
        'NATIVE_PARSER_FIXTURE ' +
            JSON.stringify({
                case: code,
                exit_code: result.code,
                stdout: result.stdout,
                stderr: result.stderr,
            }),
    );
}

const unsafeFinding = await runNative({
    type: 'message',
    role: 'assistant',
    id: 'msg_unsafe_finding',
    status: 'completed',
    content: [
        {
            type: 'output_text',
            text: JSON.stringify({
                summary: 'Synthetic finding with an unsafe path.',
                outcome: 'findings',
                findings: [
                    {
                        path: '../outside',
                        line: 1,
                        side: 'RIGHT',
                        severity: 'high',
                        explanation: 'SYNTHETIC_SECRET_UNSAFE_FINDING',
                        evidence: 'Synthetic evidence.',
                    },
                ],
                tests: [],
            }),
            annotations: [],
        },
    ],
});
assert.equal(unsafeFinding.code, 0);
assert.equal(unsafeFinding.requests.length, 1);
console.log(
    'NATIVE_PARSER_FIXTURE ' +
        JSON.stringify({
            case: 'unsafe_finding_path',
            exit_code: unsafeFinding.code,
            stdout: unsafeFinding.stdout,
            stderr: unsafeFinding.stderr,
        }),
);

await rm('/tmp/work', { recursive: true, force: true });

const { runCodeModeBoundary } = await import('./code-mode.mjs');
await runCodeModeBoundary({ runNative, home, canary, patches });

await import('./protocol.mjs');
