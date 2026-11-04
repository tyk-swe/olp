import { readdirSync } from 'node:fs';
import { basename, join } from 'node:path';

const layerFolderNames = new Set([
  'api',
  'clients',
  'components',
  'controller',
  'controllers',
  'dto',
  'dtos',
  'forms',
  'handler',
  'handlers',
  'helper',
  'helpers',
  'json',
  'lists',
  'middleware',
  'models',
  'openapi',
  'repositories',
  'repository',
  'sdk',
  'service',
  'services',
  'stores',
  'types',
  'util',
  'utils'
]);

const layerFileSuffixes = [
  'controller',
  'dto',
  'handler',
  'helper',
  'middleware',
  'model',
  'models',
  'repo',
  'repository',
  'service',
  'store',
  'util'
];

const scannedRoots = [
  '.',
  'cmd',
  'console/src/lib',
  'console/src/routes',
  'continuation',
  'guardrails',
  'inference',
  'internal',
  'management',
  'plugin',
  'tests'
];

const skippedFolders = new Set(['.git', 'node_modules', 'testdata']);

function isLayerFile(name) {
  if (!name.endsWith('.go') || name.endsWith('.gen.go')) {
    return false;
  }
  const stem = name.slice(0, -'.go'.length).replace(/_test$/, '');
  return layerFileSuffixes.some(
    (suffix) => stem === suffix || stem.endsWith(`_${suffix}`)
  );
}

export function layerViolations(entries) {
  return entries
    .filter((entry) => {
      const name = basename(entry.path);
      return entry.isDirectory
        ? layerFolderNames.has(name) || entry.path === 'plugins'
        : isLayerFile(name);
    })
    .map((entry) => entry.path)
    .sort();
}

function walk(directory, listDirectory) {
  return listDirectory(directory).flatMap((entry) => {
    if (entry.isDirectory && skippedFolders.has(entry.name)) {
      return [];
    }
    const path = directory === '.' ? entry.name : join(directory, entry.name);
    const current = { isDirectory: entry.isDirectory, path };
    return entry.isDirectory && directory !== '.'
      ? [current, ...walk(path, listDirectory)]
      : [current];
  });
}

export function listStructureEntries(
  listDirectory = (directory) => {
    try {
      return readdirSync(directory, { withFileTypes: true }).map((entry) => ({
        isDirectory: entry.isDirectory(),
        name: entry.name
      }));
    } catch {
      return [];
    }
  }
) {
  const seen = new Set();
  return scannedRoots
    .flatMap((root) => walk(root, listDirectory))
    .filter((entry) => {
      const duplicate = seen.has(entry.path);
      seen.add(entry.path);
      return !duplicate && entry.path !== '.';
    });
}

export function checkStructure(entries = listStructureEntries()) {
  const violations = layerViolations(entries);
  if (violations.length > 0) {
    throw new Error(
      `layer-first structure remains at ${violations.join(', ')}. Name the folder or file for the capability module that owns the behavior. See docs/adr/0004-capability-named-folders.md`
    );
  }
  return entries.length;
}
