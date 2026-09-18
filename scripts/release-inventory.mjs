#!/usr/bin/env node
import { readFileSync, writeFileSync, existsSync, readdirSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';

const read = (path) => JSON.parse(readFileSync(path, 'utf8'));
const sha256 = (path) => createHash('sha256').update(readFileSync(path)).digest('hex');
const unique = (values) => [...new Set(values)];
const testsFor = (behaviors) => unique(behaviors.flatMap((item) => item.tests));

// Reviewed evidence is never inferred from a filename or milestone owner.
// Named Go tests must still exist, even when their containing file survives.
export function reconcileEvidence(frozen, manifest) {
  const expected = {
    referenceSuites: frozen.suites.map((item) => item.path),
    managementOperations: frozen.management.map((item) => item.operationId),
    inferenceOperations: unique(frozen.inference.map((item) => item.operation)),
    commands: frozen.cli.map((item) => item.command)
  };
  const indexes = Object.fromEntries(Object.entries(expected).map(([field, keys]) =>
    [field, new Map(keys.map((key) => [key, []]))]
  ));
  const ids = new Set();
  for (const behavior of manifest.behaviors) {
    if (!behavior.id || ids.has(behavior.id) || !behavior.behavior || !behavior.tests?.length)
      throw new Error('Evidence needs unique IDs, a behavior description and successor tests');
    ids.add(behavior.id);
    if (behavior.disposition === 'replaced harness' && !behavior.reason)
      throw new Error('Harness retirement needs a reason: ' + behavior.id);
    for (const reference of behavior.tests) {
      const [path, name] = reference.split('#');
      if (!existsSync(path)) throw new Error('Missing evidence: ' + reference);
      if (path.endsWith('_test.go') && !name && behavior.disposition !== 'replaced harness')
        throw new Error('Name the successor Go test: ' + reference);
      if (name) {
        const names = [...readFileSync(path, 'utf8').matchAll(/^func (Test\w+)\(/gm)].map((match) => match[1]);
        if (!names.includes(name)) throw new Error('Missing test symbol: ' + reference);
      }
    }
    for (const [field, index] of Object.entries(indexes)) {
      for (const key of behavior[field] ?? []) {
        if (!index.has(key)) throw new Error('Unknown frozen ' + field + ': ' + key);
        index.get(key).push(behavior);
      }
    }
  }
  for (const [field, index] of Object.entries(indexes)) {
    for (const [key, behaviors] of index) {
      if (!behaviors.length) throw new Error('Unmapped frozen ' + field + ': ' + key);
    }
  }
  return indexes;
}

function main() {
  const frozen = read('docs/roadmap/evidence/reference-inventory.json');
  const mappingPath = 'docs/roadmap/evidence/release-behaviors.json';
  const evidence = reconcileEvidence(frozen, read(mappingPath));
  const contract = read('openapi/management.json');
  const operations = Object.entries(contract.paths).flatMap(([path, methods]) =>
    Object.entries(methods).filter(([method]) => /^(get|post|put|patch|delete|options|head)$/.test(method))
      .map(([method, value]) => ({ method: method.toUpperCase(), path, operationId: value.operationId }))
  );
  const files = ['internal', 'tests/integration'].flatMap((root) =>
    readdirSync(root, { recursive: true }).filter((path) => path.endsWith('_test.go')).map((path) => root + '/' + path)
  );
  const management = frozen.management.map((item) => {
    if (!operations.some((op) => op.method === item.method && op.path === item.path && op.operationId === item.operationId))
      throw new Error('Lost frozen operation ' + item.operationId);
    const behaviors = evidence.managementOperations.get(item.operationId);
    return { ...item, status: 'implemented', referenceTests: item.tests, behaviors: behaviors.map((entry) => entry.id), tests: testsFor(behaviors) };
  });
  for (const fixture of frozen.fixtures) if (!existsSync(fixture.path) || sha256(fixture.path) !== fixture.sha256)
    throw new Error('Frozen neutral fixture changed: ' + fixture.path);

  // Compare the authoritative Go certification boundary with independent frozen
  // tuples before generating any "implemented" inference records.
  const capabilityCheck = 'internal/providers/frozen_capabilities_test.go#TestFrozenCertificationMatrix';
  execFileSync('go', ['test', './internal/providers', '-run', '^TestFrozenCertificationMatrix$', '-count=1'], { stdio: 'inherit' });
  const referenceSuites = frozen.suites.map((item) => {
    const behaviors = evidence.referenceSuites.get(item.path);
    return {
      ...item,
      disposition: unique(behaviors.map((entry) => entry.disposition ?? 'ported')).join('; '),
      behaviors: behaviors.map((entry) => entry.id),
      tests: testsFor(behaviors)
    };
  });
  const output = {
    applicationVersion: read('package.json').version,
    frozenRustReference: frozen.reference,
    contract: { path: 'openapi/management.json', sha256: sha256('openapi/management.json'), operations: operations.length },
    behaviorMapping: { path: mappingPath, sha256: sha256(mappingPath) },
    capabilityCheck,
    management,
    inference: frozen.inference.map((item) => {
      const behaviors = evidence.inferenceOperations.get(item.operation);
      return { ...item, status: 'implemented', behaviors: behaviors.map((entry) => entry.id), tests: unique([capabilityCheck, ...testsFor(behaviors)]) };
    }),
    inferenceEndpoints: frozen.inferenceEndpoints,
    cli: frozen.cli.map((item) => {
      const behaviors = evidence.commands.get(item.command);
      return { ...item, status: 'implemented', behaviors: behaviors.map((entry) => entry.id), tests: testsFor(behaviors) };
    }),
    fixtures: frozen.fixtures.map((item) => ({ ...item, goCoverage: 'retained byte-for-byte; semantic protocol/routing/egress suites' })),
    referenceSuites,
    goSuites: files.sort(),
    browserJourneys: ['console/playwright.config.ts', 'console/playwright.journeys.config.ts'],
    qualification: 'release-qualification.md',
    qualificationPolicy: 'Implemented records reconciled operations and certification tuples. Named successor tests and harness retirement reasons live in release-behaviors.json; their passing runs, candidate identity, platform and limitations are recorded separately. Paid provider tests are separate.'
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
  console.log('Reconciled ' + management.length + ' management operations, ' + output.inference.length + ' inference tuples, ' + referenceSuites.length + ' explicit suite mappings, ' + frozen.fixtures.length + ' unchanged fixtures and ' + modules.length + ' modules.');
}

if (import.meta.main) main();
