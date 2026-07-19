import tsParser from '@typescript-eslint/parser';
import svelte from 'eslint-plugin-svelte';
import globals from 'globals';

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
    files: ['**/*.{ts,tsx}'],
    languageOptions: {
      parser: tsParser,
      parserOptions: { sourceType: 'module' },
      globals: { ...globals.browser, ...globals.node }
    }
  },
  {
    files: ['src/**/*.svelte'],
    languageOptions: {
      globals: globals.browser,
      parserOptions: { parser: tsParser }
    },
    rules: {
      'svelte/no-navigation-without-resolve': ['error', { ignoreLinks: true }]
    }
  }
];
