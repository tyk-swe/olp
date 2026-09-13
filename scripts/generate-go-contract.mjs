#!/usr/bin/env node
import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// oapi-codegen 2.8.0 cannot merge the frozen RoutingPreferences defaults.
// Defaults are annotations, not wire types. Strip only schema annotations in a
// disposable generator input; the authoritative served document stays intact.
const document = JSON.parse(readFileSync('openapi/management.json', 'utf8'));
function removeDefaults(schema) {
  if (!schema || typeof schema !== 'object' || Array.isArray(schema)) return;
  delete schema.default;
  for (const key of [
    'properties',
    'patternProperties',
    '$defs',
    'dependentSchemas'
  ]) {
    for (const value of Object.values(schema[key] ?? {})) removeDefaults(value);
  }
  for (const key of [
    'items',
    'additionalProperties',
    'not',
    'if',
    'then',
    'else',
    'contains',
    'propertyNames'
  ])
    removeDefaults(schema[key]);
  for (const key of ['allOf', 'anyOf', 'oneOf', 'prefixItems']) {
    for (const value of schema[key] ?? []) removeDefaults(value);
  }
}
for (const schema of Object.values(document.components.schemas))
  removeDefaults(schema);
const scratch = mkdtempSync(join(tmpdir(), 'olp-contract-'));
try {
  const input = join(scratch, 'management.json');
  writeFileSync(input, JSON.stringify(document));
  execFileSync(
    'go',
    ['tool', 'oapi-codegen', '--config', 'openapi/oapi-codegen.yaml', input],
    { stdio: 'inherit' }
  );
} finally {
  rmSync(scratch, { recursive: true, force: true });
}
