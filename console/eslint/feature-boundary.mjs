import path from 'node:path';

const lib = path.resolve(import.meta.dirname, '../src/lib');
const features = path.join(lib, 'features');

/** @param {string} file */
const featureOf = (file) => {
  const [feature, ...rest] = path.relative(features, file).split(path.sep);
  return feature === '..' || rest.length === 0 ? null : feature;
};
/** @param {string} file */
const layerOf = (file) => {
  const [layer, ...rest] = path.relative(lib, file).split(path.sep);
  return rest.length === 0 ? '.' : layer;
};
/** @param {string} layer */
const layerName = (layer) => (layer === '.' ? 'src/lib/*.ts' : `${layer}/`);
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
/** @param {string} file */
const outsideLib = (file) => path.relative(lib, file).startsWith('..');

/** @typedef {Parameters<NonNullable<import('eslint').Rule.RuleListener['ImportExpression']>>[0]['source']} Source */

/** @type {import('eslint').Rule.RuleModule} */
export default {
  meta: {
    type: 'problem',
    schema: [
      {
        type: 'object',
        properties: {
          layers: {
            type: 'array',
            items: { type: 'string' },
            uniqueItems: true
          }
        },
        required: ['layers'],
        additionalProperties: false
      }
    ],
    defaultOptions: [{ layers: [] }],
    messages: {
      shared:
        'Shared layers must not import a feature. Move the shared part into a shared layer or pass it in.',
      entry: 'Import another feature only through its index.ts.',
      layer:
        '{{from}} is below {{to}} in the shared layer order and must not import it. Move the shared part down a layer or pass it in.'
    }
  },
  create(context) {
    const from = context.filename;
    if (outsideLib(from)) return {};
    const owner = featureOf(from);
    /** @type {readonly string[]} */
    const layers = context.options[0].layers;
    /** @param {string} file */
    const rank = (file) => {
      const position = layers.indexOf(layerOf(file));
      return position === -1 ? layers.length : position;
    };
    /** @param {{ source?: Source | null }} node */
    const check = (node) => {
      if (node.source?.type !== 'Literal') return;
      if (typeof node.source.value !== 'string') return;
      const target = resolve(from, node.source.value);
      if (!target || outsideLib(target)) return;
      const feature = featureOf(target);
      if (feature === null) {
        if (owner === null && rank(from) < rank(target))
          context.report({
            node: node.source,
            messageId: 'layer',
            data: {
              from: layerName(layerOf(from)),
              to: layerName(layerOf(target))
            }
          });
        return;
      }
      if (feature === owner) return;
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
