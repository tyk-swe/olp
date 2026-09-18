#!/usr/bin/env node
import { readFileSync, writeFileSync, existsSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';

const read = (path) => JSON.parse(readFileSync(path, 'utf8'));
const sha256 = (path) => createHash('sha256').update(readFileSync(path)).digest('hex');
const frozen = read('docs/roadmap/evidence/reference-inventory.json');
const contract = read('openapi/management.json');
const operations = Object.entries(contract.paths).flatMap(([path, methods]) =>
  Object.entries(methods).filter(([method]) => /^(get|post|put|patch|delete|options|head)$/.test(method))
    .map(([method, value]) => ({ method: method.toUpperCase(), path, operationId: value.operationId }))
);
const behavior = {
  M1: ['tests/integration/process_test.go', 'tests/integration/release_contract_test.go'],
  M2: ['tests/integration/access_test.go', 'tests/integration/access_races_test.go', 'tests/integration/oidc_test.go'],
  M3: ['internal/gateway/gateway_test.go', 'tests/integration/provider_parity_test.go', 'tests/integration/gateway_test.go'],
  M4: ['tests/integration/m4_process_test.go', 'tests/integration/m4_recovery_test.go'],
  M5: ['internal/protocols/parity_test.go', 'internal/protocols/stream_parity_test.go', 'internal/connectors/auth_test.go'],
  M6: ['internal/gateway/media_system_test.go', 'internal/gateway/media_restore_test.go', 'internal/media/capacity_system_test.go']
};
// Fail on renamed evidence instead of publishing dangling qualification claims.
const files = execFileSync('rg', ['--files', 'internal', 'tests/integration'], { encoding: 'utf8' }).trim().split('\n');
for (const path of Object.values(behavior).flat()) if (!existsSync(path)) throw new Error(`Missing evidence: ${path}`);
const coverage = (owner) => [...new Set((owner.match(/M[1-6]/g) ?? []).flatMap((milestone) => behavior[milestone]))];
const management = frozen.management.map((item) => {
  if (!operations.some((op) => op.method === item.method && op.path === item.path && op.operationId === item.operationId))
    throw new Error(`Lost frozen operation ${item.operationId}`);
  return { ...item, status: 'implemented', referenceTests: item.tests, tests: ['tests/integration/release_contract_test.go', ...coverage(item.owner)] };
});
for (const fixture of frozen.fixtures) if (!existsSync(fixture.path) || sha256(fixture.path) !== fixture.sha256)
  throw new Error(`Frozen neutral fixture changed: ${fixture.path}`);
const suiteCoverage = (path) => {
  if (path.startsWith('console/') || path.startsWith('tests/sdk-')) return [path.replace('rust-hosted-console.spec.ts', 'hosted-console.spec.ts')];
  if (/media/.test(path)) return behavior.M6;
  if (/oidc|identity|invitation|api_key|idempotency|master_key/.test(path)) return [...behavior.M2, 'tests/integration/master_key_rotation_test.go'];
  if (/^tests\/ha[/.]|distributed|spend|consumer|worker|batched/.test(path)) return [...behavior.M4, 'tests/integration/m4_isolation_test.go'];
  if (/ssrf/.test(path)) return ['internal/egress/egress_test.go', 'internal/gateway/endpoint_test.go'];
  if (/protocol|conformance|stream|sse|anthropic_gemini/.test(path)) return [...behavior.M3, ...behavior.M5];
  if (/operations|telemetry|otlp/.test(path)) return ['internal/telemetry/tracing_test.go', 'tests/integration/reports_test.go', 'internal/observability/handler_test.go'];
  if (/contract|public_interface|data_safety/.test(path)) return ['tests/integration/contracts_test.go', 'tests/integration/release_contract_test.go', 'tests/integration/process_test.go'];
  if (/routing|route_|runtime/.test(path)) return ['tests/integration/routing_policy_parity_test.go', 'tests/integration/authority_lifecycle_test.go', 'internal/runtime/plan_parity_test.go'];
  if (/configuration|provider|persistence_correctness/.test(path)) return [...behavior.M2, ...behavior.M3];
  // The remaining main/common/support modules are test orchestration and fixtures.
  if (/main\.rs|common|support/.test(path)) return ['scripts/integration.sh', 'internal/testutil/process.go', 'tests/integration/access_test.go'];
  throw new Error(`Unreconciled reference suite ${path}`);
};
const referenceSuites = frozen.suites.map((item) => {
  const tests = suiteCoverage(item.path);
  for (const test of tests) if (!existsSync(test)) throw new Error(`Missing suite successor: ${test}`);
  return { ...item, disposition: item.path.endsWith('.rs') ? 'ported by behavior group' : 'retained', tests };
});
const output = {
  applicationVersion: read('package.json').version,
  frozenRustReference: frozen.reference,
  contract: { path: 'openapi/management.json', sha256: sha256('openapi/management.json'), operations: operations.length },
  management,
  inference: frozen.inference.map((item) => ({ ...item, status: 'implemented', tests: coverage(item.owner) })),
  inferenceEndpoints: frozen.inferenceEndpoints,
  cli: frozen.cli.map((item) => ({ ...item, status: 'implemented', tests: ['tests/integration/process_test.go', 'tests/integration/recovery_test.go'] })),
  fixtures: frozen.fixtures.map((item) => ({ ...item, goCoverage: 'retained byte-for-byte; semantic protocol/routing/egress suites' })),
  referenceSuites,
  goSuites: files.filter((file) => file.endsWith('_test.go')).sort(),
  browserJourneys: ['console/playwright.config.ts', 'console/playwright.journeys.config.ts'],
  qualification: 'release-qualification.md',
  qualificationPolicy: 'Implemented identifies code ownership. Passing runs, candidate identity, platform and limitations are recorded separately; paid provider tests are separate.'
};
writeFileSync('docs/roadmap/evidence/release-inventory.json', JSON.stringify(output, null, 2) + '\n');
const modules = execFileSync('go', ['list', '-m', '-f', '{{if not .Main}}{{.Path}} {{.Version}} {{.Indirect}}{{end}}', 'all'], { encoding: 'utf8' }).trim().split('\n').filter(Boolean).map((line) => {
  const [path, version, indirect] = line.split(' '); return { path, version, indirect: indirect === 'true' };
});
writeFileSync('docs/roadmap/evidence/release-dependencies.json', JSON.stringify({
  counting: 'Whole Go module graph includes runtime, test and tool dependencies. Native FFI lock is a conservative superset, inventoried separately.',
  modules, directCount: modules.filter((m) => !m.indirect).length, resolvedCount: modules.length,
  native: '../../../deploy/native/inventory.json',
  javascriptSDKs: read('tests/sdk-smoke/package.json').dependencies,
  pythonSDKs: readFileSync('tests/sdk-smoke-python/pyproject.toml', 'utf8').match(/(?:anthropic|google-genai|openai)==[^"\s]+/g)
}, null, 2) + '\n');
writeFileSync('docs/roadmap/evidence/release-module-graph.txt', execFileSync('go', ['mod', 'graph']));
console.log(`Reconciled ${management.length} management operations, ${output.inference.length} inference tuples, ${frozen.fixtures.length} unchanged fixtures and ${modules.length} modules.`);
