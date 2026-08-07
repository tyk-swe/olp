import js from '@eslint/js';
import svelte from 'eslint-plugin-svelte';
import globals from 'globals';
import tseslint from 'typescript-eslint';
import { buildFeatureBoundaryConfigs } from './feature-boundaries.mjs';

const tsFiles = ['**/*.{ts,tsx}'];

export default [
  {
    ignores: [
      '.svelte-kit/**',
      'build/**',
      'node_modules/**',
      'playwright-report/**',
      'storybook-static/**',
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
  ...buildFeatureBoundaryConfigs(new URL('src/lib/features', import.meta.url).pathname, new URL('.', import.meta.url).pathname),
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
              message: 'Import from ../playwright so browser runtime failures fail the test.'
            }
          ]
        }
      ]
    }
  }
];
