import path from 'node:path';

export const root = path.resolve(import.meta.dirname, '..');
export const lib = path.join(root, 'src/lib');
export const features = path.join(lib, 'features');
const testSupport = path.join(root, 'tests/unit/support');

/** @param {string} file */
export const featureOf = (file) => {
  const [feature, ...rest] = path.relative(features, file).split(path.sep);
  return feature === '..' || rest.length === 0 ? null : feature;
};
/**
 * @param {string} from
 * @param {string} specifier
 */
const resolve = (from, specifier) => {
  if (/^[#$]lib\//.test(specifier)) return path.join(lib, specifier.slice(5));
  if (specifier.startsWith('#test/'))
    return path.join(testSupport, specifier.slice(6));
  if (specifier.startsWith('.'))
    return path.resolve(path.dirname(from), specifier);
  return null;
};

/** @typedef {Parameters<NonNullable<import('eslint').Rule.RuleListener['ImportExpression']>>[0]['source']} Source */

/**
 * @param {string} from
 * @param {(source: Source, target: string) => void} check
 * @returns {import('eslint').Rule.RuleListener}
 */
export const onLocalImport = (from, check) => {
  /** @param {{ source?: Source | null }} node */
  const visit = (node) => {
    if (node.source?.type !== 'Literal') return;
    if (typeof node.source.value !== 'string') return;
    const target = resolve(from, node.source.value);
    if (target) check(node.source, target);
  };
  return {
    ImportDeclaration: visit,
    ExportNamedDeclaration: visit,
    ExportAllDeclaration: visit,
    ImportExpression: visit,
    TSImportType: visit
  };
};
