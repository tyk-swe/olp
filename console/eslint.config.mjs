import path from 'node:path';
import { fileURLToPath } from 'node:url';

import js from '@eslint/js';
import svelte from 'eslint-plugin-svelte';
import globals from 'globals';
import tseslint from 'typescript-eslint';

const tsFiles = ['**/*.{ts,tsx}'];

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
          crossFeature:
            'Feature "{{sourceFeature}}" must not import feature "{{targetFeature}}". Move shared code to a neutral $lib module or expose the behavior through an API boundary.'
        }
      },
      create(context) {
        const filename = context.filename ?? context.getFilename?.();
        if (!filename) return {};

        const sourceFeature = featureForPath(filename);
        if (!sourceFeature) return {};

        function checkImport(node) {
          const specifier = node.source?.value;
          if (typeof specifier !== 'string') return;

          const targetFeature = importedFeature(filename, specifier);
          if (targetFeature && targetFeature !== sourceFeature) {
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
    files: ['src/lib/features/**/*.{svelte,ts,tsx}'],
    plugins: { olp: olpBoundaries },
    rules: {
      'olp/no-cross-feature-imports': 'error'
    }
  },
  {
    files: ['src/**/*.svelte'],
    languageOptions: {
      globals: globals.browser,
      parserOptions: { parser: tseslint.parser }
    },
    rules: {
      'svelte/no-navigation-without-resolve': ['error', { ignoreLinks: true }]
    }
  },
  {
    files: ['tests/e2e/**/*.ts', 'tests/integration/**/*.ts'],
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
