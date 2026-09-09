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
    files: ['tests/journeys/**/*.ts'],
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
