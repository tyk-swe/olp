import path from 'node:path';

const lib = path.resolve(import.meta.dirname, '../src/lib');
const features = path.join(lib, 'features');

/** @param {string} file */
const featureOf = (file) => {
  const [feature, ...rest] = path.relative(features, file).split(path.sep);
  return feature === '..' || rest.length === 0 ? null : feature;
};
/**
 * @param {string} from
 * @param {string} specifier
 */
const resolve = (from, specifier) => {
  if (specifier.startsWith('#lib/')) return path.join(lib, specifier.slice(5));
  if (specifier.startsWith('.'))
    return path.resolve(path.dirname(from), specifier);
  return null;
};
/** @param {string} file */
const isEntry = (file) => /^index\.(js|ts)$/.test(path.basename(file));

/** @typedef {Parameters<NonNullable<import('eslint').Rule.RuleListener['ImportExpression']>>[0]['source']} Source */

/** @type {import('eslint').Rule.RuleModule} */
export default {
  meta: {
    type: 'problem',
    schema: [],
    messages: {
      shared:
        'Shared layers must not import a feature. Move the shared part into a shared layer or pass it in.',
      entry: 'Import another feature only through its index.ts.'
    }
  },
  create(context) {
    const from = context.filename;
    if (path.relative(lib, from).startsWith('..')) return {};
    const owner = featureOf(from);
    /** @param {{ source?: Source | null }} node */
    const check = (node) => {
      if (node.source?.type !== 'Literal') return;
      if (typeof node.source.value !== 'string') return;
      const target = resolve(from, node.source.value);
      const feature = target && featureOf(target);
      if (!feature || feature === owner) return;
      if (owner === null)
        return context.report({ node: node.source, messageId: 'shared' });
      const direct = path.dirname(target) === path.join(features, feature);
      if (!direct || !isEntry(target))
        context.report({ node: node.source, messageId: 'entry' });
    };
    return {
      ImportDeclaration: check,
      ExportNamedDeclaration: check,
      ExportAllDeclaration: check,
      ImportExpression: check
    };
  }
};
