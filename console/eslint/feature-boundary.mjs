import path from 'node:path';

const root = path.resolve(import.meta.dirname, '..');
const lib = path.join(root, 'src/lib');
const routes = path.join(root, 'src/routes');
const features = path.join(lib, 'features');
const testSupport = path.join(root, 'tests/unit/support');

/**
 * @param {string} directory
 * @param {string} file
 */
const outside = (directory, file) =>
  path.relative(directory, file).startsWith('..');
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
/** @param {string} file */
const isTest = (file) => {
  const parts = path.relative(root, file).split(path.sep);
  return (
    /\.test(\.[jt]s)?$/.test(file) ||
    parts[0] === 'tests' ||
    (parts[0] === 'src' && parts.includes('test'))
  );
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
/** @param {string} file */
const isEntry = (file) => /^index\.(js|ts)$/.test(path.basename(file));

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
          },
          apiClient: { type: 'string' }
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
        '{{from}} is below {{to}} in the shared layer order and must not import it. Move the shared part down a layer or pass it in.',
      test: 'Only test files import a test file or a test folder. Move what production code needs out of it.',
      routes:
        'src/lib must not import a route. Move the shared part into src/lib.',
      apiClient:
        'Call the management API from the feature api module, not a component.'
    }
  },
  create(context) {
    const from = context.filename;
    const owner = featureOf(from);
    /** @type {{ layers: readonly string[], apiClient?: string }} */
    const { layers, apiClient } = context.options[0];
    const client = apiClient && path.join(lib, apiClient);
    /** @param {string} file */
    const rank = (file) => {
      const position = layers.indexOf(layerOf(file));
      return position === -1 ? layers.length : position;
    };
    /**
     * @param {string} target
     * @returns {{ messageId: string, data?: Record<string, string> } | null}
     */
    const violation = (target) => {
      if (isTest(target)) return { messageId: 'test' };
      if (target.replace(/\.[jt]s$/, '') === client)
        return { messageId: 'apiClient' };
      if (outside(lib, from)) return null;
      if (!outside(routes, target)) return { messageId: 'routes' };
      if (outside(lib, target)) return null;
      const feature = featureOf(target);
      if (feature === null && owner === null && rank(from) < rank(target))
        return {
          messageId: 'layer',
          data: {
            from: layerName(layerOf(from)),
            to: layerName(layerOf(target))
          }
        };
      if (feature === null || feature === owner) return null;
      if (owner === null) return { messageId: 'shared' };
      const direct = path.dirname(target) === path.join(features, feature);
      return direct && isEntry(target) ? null : { messageId: 'entry' };
    };
    /** @param {{ source?: Source | null }} node */
    const check = (node) => {
      if (node.source?.type !== 'Literal') return;
      if (typeof node.source.value !== 'string') return;
      const target = resolve(from, node.source.value);
      const found = target && violation(target);
      if (found) context.report({ node: node.source, ...found });
    };
    return {
      ImportDeclaration: check,
      ExportNamedDeclaration: check,
      ExportAllDeclaration: check,
      ImportExpression: check,
      TSImportType: check
    };
  }
};
