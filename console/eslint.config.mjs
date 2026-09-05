import path from 'node:path';
import { fileURLToPath } from 'node:url';

import js from '@eslint/js';
import svelte from 'eslint-plugin-svelte';
import globals from 'globals';
import tseslint from 'typescript-eslint';

const tsFiles = ['**/*.{ts,tsx}'];
const svelteFiles = ['src/**/*.svelte'];
// `<script lang="ts">` blocks are held to the same recommended rules as the
// .ts modules; without this, unused imports and locals in components are
// invisible to the linter.
const typeScriptRecommendedRules = tseslint.configs.recommended.reduce(
  (rules, config) => ({ ...rules, ...config.rules }),
  {}
);

const consoleRoot = path.dirname(fileURLToPath(import.meta.url));
const featureRoot = path.join(consoleRoot, 'src', 'lib', 'features');

function featureForPath(filename) {
  const relative = path.relative(featureRoot, path.resolve(filename));
  if (
    !relative ||
    relative === '..' ||
    relative.startsWith(`..${path.sep}`) ||
    path.isAbsolute(relative)
  ) {
    return null;
  }
  return relative.split(path.sep)[0] ?? null;
}

function importedFeature(filename, specifier) {
  const featureAlias = '$lib/features/';
  if (specifier.startsWith(featureAlias)) {
    return featureForPath(
      path.resolve(featureRoot, specifier.slice(featureAlias.length))
    );
  }
  if (specifier.startsWith('.')) {
    return featureForPath(path.resolve(path.dirname(filename), specifier));
  }
  return null;
}

const olpBoundaries = {
  rules: {
    'no-cross-feature-imports': {
      meta: {
        type: 'problem',
        schema: [],
        messages: {
          neutral: 'Shared $lib modules must not import feature modules.',
          apiUi:
            'API modules must not own Svelte state or depend on UI modules.',
          forwarding:
            'Import from the defining module instead of forwarding exports.',
          crossFeature:
            'Feature "{{sourceFeature}}" must not import feature "{{targetFeature}}". Move shared code to a neutral $lib module or expose the behavior through an API boundary.'
        }
      },
      create(context) {
        const filename = context.filename ?? context.getFilename?.();
        if (!filename) return {};

        const sourceFeature = featureForPath(filename);
        const relative = path.relative(
          path.join(consoleRoot, 'src', 'lib'),
          filename
        );
        const neutral =
          !sourceFeature &&
          !relative.startsWith('..') &&
          !path.isAbsolute(relative);
        const api = relative.startsWith(`api${path.sep}`);

        function checkImport(node) {
          if (node.type.startsWith('Export')) {
            const scope = context.sourceCode.getScope(node);
            const forwarded =
              node.source ||
              node.specifiers?.some((specifier) =>
                scope.variables.some(
                  (variable) =>
                    variable.name === specifier.local?.name &&
                    variable.defs.some(
                      (definition) => definition.type === 'ImportBinding'
                    )
                )
              );
            if (forwarded) context.report({ node, messageId: 'forwarding' });
          }
          const specifier = node.source?.value;
          if (typeof specifier !== 'string') return;

          const targetFeature = importedFeature(filename, specifier);
          const target = specifier.startsWith('$lib/')
            ? path.resolve(consoleRoot, 'src', 'lib', specifier.slice(5))
            : specifier.startsWith('.')
              ? path.resolve(path.dirname(filename), specifier)
              : null;
          const targetRelative =
            target &&
            path.relative(path.join(consoleRoot, 'src', 'lib'), target);
          if (
            api &&
            (specifier === 'svelte' ||
              specifier.startsWith('svelte/') ||
              /^(lists|forms|components)(\/|\\)/.test(targetRelative ?? '') ||
              target?.endsWith('.svelte') ||
              target?.endsWith('.svelte.ts'))
          ) {
            context.report({ node, messageId: 'apiUi' });
          }
          if (targetFeature && neutral) {
            context.report({ node, messageId: 'neutral' });
          } else if (
            targetFeature &&
            sourceFeature &&
            targetFeature !== sourceFeature
          ) {
            context.report({
              node,
              messageId: 'crossFeature',
              data: { sourceFeature, targetFeature }
            });
          }
        }

        return {
          ExportAllDeclaration: checkImport,
          ExportNamedDeclaration: checkImport,
          ImportDeclaration: checkImport,
          ImportExpression: checkImport
        };
      }
    }
  }
};

export default [
  {
    ignores: [
      '.svelte-kit/**',
      'build/**',
      'node_modules/**',
      'playwright-report/**',
      'test-results/**',
      'src/lib/api/schema.d.ts'
    ]
  },
  ...svelte.configs['flat/recommended'],
  {
    ...js.configs.recommended,
    files: tsFiles
  },
  ...tseslint.configs.recommended.map((config) => ({
    ...config,
    files: tsFiles
  })),
  {
    files: tsFiles,
    languageOptions: {
      parser: tseslint.parser,
      parserOptions: { sourceType: 'module' },
      globals: { ...globals.browser, ...globals.node }
    }
  },
  {
    files: ['src/lib/**/*.{svelte,ts,tsx}'],
    plugins: { olp: olpBoundaries },
    rules: {
      'olp/no-cross-feature-imports': 'error'
    }
  },
  {
    files: svelteFiles,
    plugins: { '@typescript-eslint': tseslint.plugin },
    languageOptions: {
      globals: globals.browser,
      parserOptions: { parser: tseslint.parser }
    },
    rules: {
      ...js.configs.recommended.rules,
      ...typeScriptRecommendedRules,
      // Runes are declared with `let` even when they are never reassigned:
      // `$props()`, `$state()`, and `$bindable()` bindings are rewritten by the
      // compiler, so `prefer-const` would fight the framework in every file.
      'prefer-const': 'off',
      'svelte/no-navigation-without-resolve': ['error', { ignoreLinks: true }]
    }
  },
  {
    files: [
      'tests/e2e/**/*.ts',
      'tests/integration/**/*.ts',
      'tests/screenshots/**/*.ts'
    ],
    rules: {
      'no-restricted-imports': [
        'error',
        {
          paths: [
            {
              name: '@playwright/test',
              message:
                'Import from ../playwright so browser runtime failures fail the test.'
            }
          ]
        }
      ]
    }
  }
];
