import { readdirSync, writeFileSync } from 'node:fs';
import { join, relative, sep } from 'node:path';

const goTestFile = (file) => file.endsWith('_test.go');
const goSourceFile = (file) => file.endsWith('.go') && !goTestFile(file);
const jsTestFile = (file) => /\.(test|spec)\.(ts|tsx|js|jsx|mts|mjs)$/.test(file);
const jsSourceFile = (file) =>
  file.startsWith('console/src/') &&
  (file.endsWith('.ts') || file.endsWith('.svelte')) &&
  !file.endsWith('.d.ts') &&
  !jsTestFile(file);
const dirOf = (file) => (file.includes('/') ? file.slice(0, file.lastIndexOf('/')) : '.');
const areaOf = (file) => file.split('/')[0];

export function summarizeInventory(files) {
  const sorted = [...files].sort();
  const goTests = sorted.filter(goTestFile);
  const jsTests = sorted.filter(jsTestFile);
  const testDirs = new Set(goTests.map(dirOf));
  const byArea = new Map(
    [...new Set([...goTests, ...jsTests].map(areaOf))]
      .sort()
      .map((area) => [
        area,
        {
          goTests: goTests.filter((f) => areaOf(f) === area).length,
          jsTests: jsTests.filter((f) => areaOf(f) === area).length
        }
      ])
  );
  const sourceByDir = Map.groupBy(sorted.filter(goSourceFile), dirOf);
  const unmappedGoPackages = [...sourceByDir]
    .filter(([dir]) => !testDirs.has(dir))
    .map(([dir, inDir]) => ({ dir, sourceFiles: inDir.length }))
    .sort((a, b) => a.dir.localeCompare(b.dir));
  const jsTestDirs = new Set(jsTests.map(dirOf));
  const jsSourceByDir = Map.groupBy(sorted.filter(jsSourceFile), dirOf);
  const unmappedJsDirs = [...jsSourceByDir]
    .filter(([dir]) => !jsTestDirs.has(dir))
    .map(([dir, inDir]) => ({ dir, sourceFiles: inDir.length }))
    .sort((a, b) => b.sourceFiles - a.sourceFiles || a.dir.localeCompare(b.dir));
  return {
    goTests: goTests.length,
    jsTests: jsTests.length,
    byArea,
    goPackages: sourceByDir.size,
    mappedGoPackages: sourceByDir.size - unmappedGoPackages.length,
    unmappedGoPackages,
    jsDirs: jsSourceByDir.size,
    mappedJsDirs: jsSourceByDir.size - unmappedJsDirs.length,
    unmappedJsDirs
  };
}

export function formatInventory(summary) {
  const areaRows = [...summary.byArea].map(
    ([area, counts]) => `| ${area} | ${counts.goTests} | ${counts.jsTests} |`
  );
  const unmappedRows = summary.unmappedGoPackages.map(
    (pkg) => `| ${pkg.dir} | ${pkg.sourceFiles} |`
  );
  const unmappedJsRows = summary.unmappedJsDirs.map(
    (pkg) => `| ${pkg.dir} | ${pkg.sourceFiles} |`
  );
  return [
    '# Test inventory',
    '',
    `Go test files: ${summary.goTests}. JS/TS test files: ${summary.jsTests}.`,
    '',
    '| Area | Go tests | JS/TS tests |',
    '|---|---:|---:|',
    ...areaRows,
    '',
    '## Go packages with source but no sibling test file',
    '',
    `Go packages with source: ${summary.goPackages}. With a sibling test file: ${summary.mappedGoPackages}.`,
    '',
    '| Package | Source files |',
    '|---|---:|',
    ...unmappedRows,
    '',
    '## Console source directories with no sibling test file',
    '',
    `Console source directories: ${summary.jsDirs}. With a sibling test file: ${summary.mappedJsDirs}.`,
    '',
    '| Directory | Source files |',
    '|---|---:|',
    ...unmappedJsRows,
    ''
  ].join('\n');
}

const excludedDirs = new Set([
  '.git',
  '.local',
  '.muse',
  '.stryker-tmp',
  '.svelte-kit',
  'build',
  'coverage',
  'node_modules'
]);

export function walk(root) {
  return readdirSync(root, { withFileTypes: true }).flatMap((entry) => {
    const path = join(root, entry.name);
    if (entry.isDirectory()) return excludedDirs.has(entry.name) ? [] : walk(path);
    return entry.isFile() ? [relative('.', path).split(sep).join('/')] : [];
  });
}

if (import.meta.main) {
  const report = formatInventory(summarizeInventory(walk('.')));
  if (process.argv[2]) writeFileSync(process.argv[2], report);
  else process.stdout.write(report);
}
