#!/usr/bin/env node
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';

const reference = '6c21dfb917c9019161348ea24b532a77b6612e6e';
const read = (path) =>
  execFileSync('git', ['show', `${reference}:${path}`], {
    encoding: 'utf8',
    maxBuffer: 16 * 1024 * 1024
  });
const files = execFileSync('git', ['ls-tree', '-r', '--name-only', reference], {
  encoding: 'utf8'
})
  .trim()
  .split('\n');
const evidence = 'docs/roadmap/evidence';
mkdirSync(evidence, { recursive: true });
const documentBytes = readFileSync('openapi/management.json');
const document = JSON.parse(documentBytes);
const hash = (data) => createHash('sha256').update(data).digest('hex');
const source = (path) =>
  `https://github.com/tyk-swe/olp/blob/${reference}/${path}`;
function owner(path) {
  if (path.endsWith('/openapi.json')) return 'M1';
  if (/media-jobs|provider-health|\/health\//.test(path)) return 'M6';
  if (/pricing|\/requests|request-metadata|\/usage/.test(path)) return 'M4';
  if (
    /routing-polic|\/routing\/|\/simulate|provider-vendors|credential-slots/.test(
      path
    )
  )
    return 'M5';
  if (/provider|route|runtime|playground/.test(path))
    return 'M3 (core), M5 (remaining options/connectors)';
  return 'M2';
}
function feature(path) {
  if (/provider/.test(path)) return 'providers';
  if (/route|routing/.test(path)) return 'routes';
  if (/pricing|requests|request-metadata|usage/.test(path)) return 'usage';
  if (/media/.test(path)) return 'media';
  if (/settings/.test(path)) return 'settings';
  if (/runtime/.test(path)) return 'runtime';
  if (/health/.test(path)) return 'observability';
  if (/playground/.test(path)) return 'inference';
  if (/openapi/.test(path)) return 'http/control';
  return 'access';
}
const functions = execFileSync(
  'git',
  ['grep', '-n', '-E', 'fn [a-z_][a-z_0-9]*', reference, '--', 'src'],
  { encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 }
)
  .trim()
  .split('\n')
  .map((line) => {
    const match = line.match(/^[^:]+:([^:]+):(\d+):.*?\bfn ([a-z_][a-z_0-9]*)/);
    return match && { path: match[1], line: match[2], name: match[3] };
  })
  .filter(Boolean);
function implementation(path, id) {
  const candidates = functions.filter((fn) => fn.name === id);
  const fn =
    candidates.find(
      (fn) =>
        fn.path.startsWith(`src/${feature(path)}/`) && /http/.test(fn.path)
    ) ??
    candidates.find((fn) => /http/.test(fn.path)) ??
    candidates[0];
  return fn
    ? `${source(fn.path)}#L${fn.line}`
    : source(`src/${feature(path)}/`);
}
function tests(path) {
  const suite = /api-keys/.test(path)
    ? 'configuration_http_postgres/api_keys'
    : /oidc/.test(path)
      ? 'oidc_http_postgres'
      : /provider/.test(path)
        ? 'configuration_http_postgres/providers'
        : /route|routing/.test(path)
          ? 'configuration_http_postgres/routes'
          : /media/.test(path)
            ? 'media_jobs_http_postgres'
            : /usage|requests|pricing|health/.test(path)
              ? 'operations_http_postgres'
              : 'identity_http_postgres';
  return [
    source(`tests/system/${suite}.rs`),
    source('tests/contract/management_contract.rs')
  ];
}
const management = Object.entries(document.paths).flatMap(([path, item]) =>
  Object.entries(item)
    .filter(([method]) =>
      /^(get|post|put|patch|delete|head|options)$/.test(method)
    )
    .map(([method, operation]) => ({
      method: method.toUpperCase(),
      path,
      operationId: operation.operationId,
      owner: owner(path),
      status: path.endsWith('/openapi.json') ? 'implemented' : 'deferred',
      source: implementation(path, operation.operationId),
      tests: tests(path)
    }))
);
const matrixPath = 'tests/conformance/provider_connectors/matrix.rs';
const matrix = read(matrixPath);
function tuples(name) {
  const block = matrix.match(
    new RegExp(`const ${name}: [\\s\\S]*?= \\[([\\s\\S]*?)\\];`)
  )?.[1];
  if (!block) throw new Error(`Missing reference matrix ${name}`);
  return [...block.matchAll(/capability!\((\w+), (\w+), (\w+)\)/g)].map(
    (m) => ({ operation: m[1], surface: m[2], transport: m[3] })
  );
}
const shared = tuples('SHARED_NATIVE_CAPABILITIES');
const inference = [
  'OpenAi',
  'Anthropic',
  'Gemini',
  'VertexAi',
  'Bedrock',
  'AzureOpenAi',
  'OpenAiCompatible'
].flatMap((provider) => {
  const capabilities =
    provider === 'OpenAiCompatible'
      ? tuples('OPENAI_COMPATIBLE_CAPABILITIES')
      : [
          ...shared,
          ...(provider === 'OpenAi'
            ? tuples('OPENAI_NATIVE_EXTRA_CAPABILITIES')
            : provider === 'AzureOpenAi'
              ? tuples('AZURE_OPENAI_EXTRA_CAPABILITIES')
              : [])
        ];
  return capabilities.map((tuple) => ({
    provider,
    ...tuple,
    owner: /Image|Speech|Transcription|Video/.test(tuple.operation)
      ? 'M6'
      : provider === 'OpenAi' &&
          tuple.surface === 'OpenAi' &&
          tuple.operation === 'Generation'
        ? 'M3'
        : 'M5',
    status: 'deferred',
    source: source(matrixPath)
  }));
});
const registryPath = 'src/inference/http/endpoint_policy/registry.rs';
const endpoints = [
  ...new Set(
    [...read(registryPath).matchAll(/(?:path|route_path):\s*"([^"]+)"/g)].map(
      (m) => m[1]
    )
  )
];
const fixtureFiles = files.filter(
  (path) => path.startsWith('tests/fixtures/') && /\.(json|sse)$/.test(path)
);
const fixtures = fixtureFiles.map((path) => ({
  path,
  sha256: hash(read(path)),
  reference: source(path),
  goCoverage: path.includes('streams')
    ? 'SSE framing; provider decoding deferred M3/M5'
    : 'retained JSON; semantic runners join in M3–M6'
}));
const suites = files
  .filter((path) =>
    /^(tests\/.*\.rs|console\/tests\/journeys\/.*\.(spec\.ts|ts)|tests\/sdk-smoke[^/]*\/.*)$/.test(
      path
    )
  )
  .map((path) => ({ path, reference: source(path) }));
const journeys = files
  .filter((path) => /^console\/tests\/journeys\/.*\.spec\.ts$/.test(path))
  .map((path) => ({
    path,
    owner: path.includes('recovery')
      ? 'M7'
      : 'M2/M3/M4/M5/M6 with APIs; final M7',
    titles: [
      ...read(path).matchAll(
        /\btest(?:\.describe)?(?:\.serial)?\(\s*['"]([^'"]+)['"]/g
      )
    ].map((m) => m[1]),
    reference: source(path)
  }));
const cli = [
  ...[
    'all',
    'gateway',
    'control',
    'worker',
    'health-probe',
    'internal-pre-stop'
  ].map((command) => ({
    command,
    owner: 'M1 foundation; M7 qualification',
    status: 'implemented'
  })),
  ...[
    'migrate',
    'doctor',
    'master-key status',
    'master-key reencrypt',
    'master-key verify-retirement'
  ].map((command) => ({
    command,
    owner: command === 'doctor' ? 'M6/M7' : 'M2',
    status: 'explicit unimplemented error'
  }))
];
const cargo = read('Cargo.toml');
const dependencySection = cargo.split('[dependencies]')[1].split(/\n\[/)[0];
const direct = [
  ...dependencySection.matchAll(/^([a-zA-Z0-9_-]+)\s*=/gm),
  ...cargo.matchAll(/^\[dependencies\.([^\]]+)\]/gm)
]
  .map((m) => m[1])
  .sort();
const resolved = [...read('Cargo.lock').matchAll(/^\[\[package\]\]/gm)].length;
if (direct.length !== 50 || resolved !== 467)
  throw new Error('Frozen Cargo counts changed unexpectedly');
const inventory = {
  reference,
  contract: {
    path: 'openapi/management.json',
    sha256: hash(documentBytes),
    dialect: document.openapi,
    operations: management.length,
    exportedWith:
      'cargo run --locked --all-features --bin export_openapi (frozen detached worktree)'
  },
  management,
  inference,
  inferenceEndpoints: endpoints,
  cli,
  journeys,
  fixtures,
  suites,
  rustDependencies: {
    counting:
      'Direct normal dependencies in [dependencies]; all resolved [[package]] entries including root/dev/transitive',
    directCount: direct.length,
    direct,
    resolvedPackageEntries: resolved
  }
};
writeFileSync(
  `${evidence}/reference-inventory.json`,
  JSON.stringify(inventory, null, 2) + '\n'
);
const rows = management
  .map(
    (row) =>
      `| ${row.method} \`${row.path}\` | \`${row.operationId}\` | ${row.owner} | ${row.status} | [source](${row.source}), [tests](${row.tests[0]}) |`
  )
  .join('\n');
writeFileSync(
  `${evidence}/management-operations.md`,
  `# Frozen management operations\n\nReference: \`${reference}\`. ${management.length} operations on ${Object.keys(document.paths).length} paths. Generated by \`node scripts/foundation-inventory.mjs\`; update status/ownership deliberately as handlers land. The current Go definition is served exactly; deferred operations return HTTP 501.\n\n| Operation | ID | Completion owner | Go status | Frozen evidence |\n| --- | --- | --- | --- | --- |\n${rows}\n`
);
console.log(
  `Inventoried ${management.length} management operations, ${inference.length} provider tuples, ${fixtures.length} JSON/SSE files, and ${suites.length} suite sources`
);
