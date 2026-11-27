/** @type {import('eslint').Rule.RuleModule} */
export default {
  meta: {
    type: 'problem',
    schema: [],
    messages: {
      dropped:
        'package.json marks only CSS as having side effects, so the build drops this import. Import a named export and call it.'
    }
  },
  create(context) {
    return {
      ImportDeclaration(node) {
        if (node.specifiers.length > 0) return;
        const source = String(node.source.value);
        if (!/^(\.|[#$]lib\/)/.test(source) || source.endsWith('.css')) return;
        context.report({ node: node.source, messageId: 'dropped' });
      }
    };
  }
};
