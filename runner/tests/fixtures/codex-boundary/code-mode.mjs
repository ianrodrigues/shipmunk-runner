import assert from 'node:assert/strict';
import { readFile, readdir } from 'node:fs/promises';

const configuration = [
    '-c',
    'model_catalog_json="/fixture/tests/models.json"',
];

function call(source) {
    return {
        type: 'custom_tool_call',
        id: 'fc_code_mode',
        call_id: 'call_code_mode',
        name: 'exec',
        input: source,
    };
}

function outputText(result) {
    const output = result.requests.at(-1).input.find(
        (item) =>
            item.type === 'custom_tool_call_output' &&
            item.call_id === 'call_code_mode',
    );
    assert.ok(output, JSON.stringify(result));
    return typeof output.output === 'string'
        ? output.output
        : output.output.map((item) => item.text ?? '').join('\n');
}

export async function runCodeModeBoundary({
    runNative: native,
    home,
    canary,
    patches,
}) {
    const runNative = (item, extra, mediate = false) =>
        native(item, extra, mediate, null, 'gpt-5.6-sol');
    const disabled = await runNative(call("text('UNEXPECTED_HOST_EXECUTION');"), [
        ...configuration,
        '-c',
        'features.code_mode_host=false',
    ]);
    assert.equal(disabled.requests.length, 2, disabled.stderr);
    assert.match(
        outputText(disabled),
        /unsupported custom tool call: exec|code.mode host is disabled/i,
    );
    assert.doesNotMatch(outputText(disabled), /UNEXPECTED_HOST_EXECUTION/);
    console.log('PASS metadata-required code mode fails closed with its host disabled');

    const security = await runNative(
        call(String.raw`
const names = ['process', 'require', 'fetch', 'Deno', 'WebSocket', 'XMLHttpRequest', 'WebAssembly'];
const globals = Object.fromEntries(names.map(name => [name, typeof globalThis[name]]));
const imports = [];
for (const specifier of ['node:fs', 'node:child_process', 'file:///profile/.codex/auth.canary', 'https://127.0.0.1/untrusted.js']) {
    try {
        await import(specifier);
        imports.push({ specifier, rejected: false });
    } catch {
        imports.push({ specifier, rejected: true });
    }
}
let imageRejected = false;
try { image('file:///profile/.codex/auth.canary'); } catch { imageRejected = true; }
text('CODE_MODE_SECURITY ' + JSON.stringify({
    globals,
    imports,
    imageRejected,
    constructorProcess: Function('return typeof process')(),
    evalRequire: eval('typeof require'),
    tools: ALL_TOOLS.map(tool => tool.name).sort(),
}));
`),
        configuration,
    );
    assert.equal(security.code, 0, security.stderr);
    assert.equal(security.requests[0].model, 'gpt-5.6-sol');
    const text = outputText(security);
    const marker = 'CODE_MODE_SECURITY ';
    assert.ok(text.includes(marker), text);
    const inspection = JSON.parse(
        text.slice(text.indexOf(marker) + marker.length).split('\n')[0],
    );
    assert.ok(
        Object.values(inspection.globals).every((value) => value === 'undefined'),
    );
    assert.ok(inspection.imports.every((entry) => entry.rejected));
    assert.equal(inspection.imageRejected, true);
    assert.equal(inspection.constructorProcess, 'undefined');
    assert.equal(inspection.evalRequire, 'undefined');
    assert.deepEqual(inspection.tools, [
        'list_mcp_resource_templates',
        'list_mcp_resources',
        'mcp__repository__repository_command',
        'read_mcp_resource',
    ]);
    console.log('PASS required code-mode host exposes no filesystem network process or import APIs');

    const staticImport = await runNative(
        call("import fs from 'node:fs'; text('UNEXPECTED_STATIC_IMPORT');"),
        configuration,
    );
    assert.equal(staticImport.code, 0, staticImport.stderr);
    assert.match(outputText(staticImport), /Unsupported import in exec/);

    for (const [name, patch] of Object.entries(patches)) {
        const patchText = JSON.stringify('*** Begin Patch\n' + patch + '*** End Patch');
        const deniedPatch = await runNative(
            call(`text(await tools.apply_patch(${patchText}));`),
            configuration,
        );
        assert.equal(deniedPatch.code, 0, deniedPatch.stderr);
        assert.match(outputText(deniedPatch), /tools.apply_patch is not a function/);
        assert.equal(await readFile(home + '/config.toml', 'utf8'), canary);
        assert.equal(
            await readFile(home + '/auth.canary', 'utf8'),
            'SYNTHETIC_CREDENTIAL_CANARY',
        );
        assert.equal((await readdir(home)).includes('evil.toml'), false);
        assert.deepEqual(await readdir('/bridge'), []);
        console.log(`PASS code-mode nested local patch ${name} cannot modify protected profile`);
    }

    const mediated = await runNative(
        call(String.raw`
const repository = ALL_TOOLS.find(tool => tool.name.endsWith('repository_command'));
text(await tools[repository.name]({ command: 'touch /profile/evil' }));
`),
        configuration,
        'nested',
    );
    assert.equal(mediated.code, 0, mediated.stderr);
    assert.deepEqual(mediated.mediated, { id: 1, command: 'touch /profile/evil' });
    assert.match(outputText(mediated), /HOST_FIXTURE_ONLY/);
    assert.equal((await readdir('/profile')).includes('evil'), false);
    assert.deepEqual(await readdir('/bridge'), []);
    console.log('PASS code-mode repository execution uses only the trusted MCP bridge');
}
