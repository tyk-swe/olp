import { readFileSync, writeFileSync } from 'node:fs';
import { walk } from './test-baseline.mjs';

export const baselinePath = 'scripts/coupling-baseline.json';

const metrics = [
  'tablePackagePairs',
  'sharedTables',
  'envOutsideConfig',
  'timeNowCalls',
  'packageVars'
];

const inScope = (path) =>
  path.startsWith('internal/') &&
  path.endsWith('.go') &&
  !path.endsWith('_test.go') &&
  !path.endsWith('.gen.go');

const occurrences = (text, pattern) => (text.match(pattern) ?? []).length;

function packageVars(text) {
  let count = 0;
  let inBlock = false;
  for (const line of text.split('\n')) {
    const sentinel = /=\s*errors\.New\(/.test(line);
    if (/^var\s+\($/.test(line)) {
      inBlock = true;
    } else if (inBlock && line === ')') {
      inBlock = false;
    } else if (inBlock && /^\s+[A-Za-z_]/.test(line) && !sentinel) {
      count += 1;
    } else if (/^var\s+[A-Za-z_]/.test(line) && !sentinel) {
      count += 1;
    }
  }
  return count;
}

export function measureCoupling(files) {
  const scoped = files.filter(({ path }) => inScope(path));
  const tablesByPackage = new Map();
  const packagesByTable = new Map();
  for (const { path, text } of scoped) {
    const pkg = path.split('/')[1];
    const tables = tablesByPackage.get(pkg) ?? new Set();
    for (const [, table] of text.matchAll(/\bolp\.([a-z_][a-z0-9_]*)/g)) {
      tables.add(table);
      const packages = packagesByTable.get(table) ?? new Set();
      packages.add(pkg);
      packagesByTable.set(table, packages);
    }
    tablesByPackage.set(pkg, tables);
  }
  const envFile = ({ path, text }) =>
    !path.startsWith('internal/config/') &&
    !path.startsWith('internal/process/') &&
    !/^package main$/m.test(text);
  return {
    tablePackagePairs: [...tablesByPackage.values()].reduce(
      (total, tables) => total + tables.size,
      0
    ),
    sharedTables: [...packagesByTable.values()].filter(
      (packages) => packages.size >= 3
    ).length,
    envOutsideConfig: scoped
      .filter(envFile)
      .reduce(
        (total, { text }) =>
          total + occurrences(text, /\bos\.(Getenv|LookupEnv)\b/g),
        0
      ),
    timeNowCalls: scoped.reduce(
      (total, { text }) => total + occurrences(text, /\btime\.Now\(\)/g),
      0
    ),
    packageVars: scoped.reduce((total, { text }) => total + packageVars(text), 0)
  };
}

export function checkCoupling(current, baseline) {
  return metrics.flatMap((metric) => {
    if (baseline[metric] === undefined)
      return [`${metric}: missing from ${baselinePath}`];
    return current[metric] > baseline[metric]
      ? [
          `${metric}: ${current[metric]} exceeds baseline ${baseline[metric]}: remove the new coupling or lower another count in the same change`
        ]
      : [];
  });
}

export function formatReport(current, baseline) {
  return metrics
    .map((metric) => {
      const base = baseline[metric];
      const delta = base === undefined ? 'no baseline' : `delta ${current[metric] - base}`;
      return `${metric}: ${current[metric]} (baseline ${base ?? '?'}, ${delta})`;
    })
    .join('\n');
}

if (import.meta.main) {
  const files = walk('.')
    .filter(inScope)
    .map((path) => ({ path, text: readFileSync(path, 'utf8') }));
  const current = measureCoupling(files);
  if (process.argv[2] === '--update') {
    writeFileSync(baselinePath, `${JSON.stringify(current, null, 2)}\n`);
    console.log(`coupling: baseline written to ${baselinePath}`);
    console.log(formatReport(current, current));
  } else if (process.argv[2] === '--report') {
    const baseline = JSON.parse(readFileSync(baselinePath, 'utf8'));
    console.log(formatReport(current, baseline));
  } else {
    const baseline = JSON.parse(readFileSync(baselinePath, 'utf8'));
    console.log(formatReport(current, baseline));
    const violations = checkCoupling(current, baseline);
    for (const violation of violations) console.error(`coupling: ${violation}`);
    if (violations.length > 0) process.exit(1);
    console.log('coupling: within baseline');
  }
}
