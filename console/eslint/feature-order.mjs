import path from 'node:path';
import { featureOf, onLocalImport } from './imports.mjs';

/** @type {import('eslint').Rule.RuleModule} */
export default {
  meta: {
    type: 'problem',
    schema: [
      {
        type: 'object',
        properties: {
          order: {
            type: 'array',
            items: { type: 'string' },
            uniqueItems: true
          }
        },
        required: ['order'],
        additionalProperties: false
      }
    ],
    defaultOptions: [{ order: [] }],
    messages: {
      later:
        '{{from}} comes before {{to}} in the feature order and must not import it. Move the shared part into a shared layer or pass it in.',
      unlisted:
        'Add {{feature}} to the feature order in eslint.config.mjs, after every feature it imports.'
    }
  },
  create(context) {
    const from = context.filename;
    const owner = featureOf(from);
    if (owner === null) return {};
    /** @type {{ order: readonly string[] }} */
    const { order } = context.options[0];
    return onLocalImport(from, (source, target) => {
      const feature = featureOf(path.join(target, 'index'));
      if (feature === null || feature === owner) return;
      const unlisted = [owner, feature].find((name) => !order.includes(name));
      if (unlisted)
        context.report({
          node: source,
          messageId: 'unlisted',
          data: { feature: unlisted }
        });
      else if (order.indexOf(feature) > order.indexOf(owner))
        context.report({
          node: source,
          messageId: 'later',
          data: { from: owner, to: feature }
        });
    });
  }
};
