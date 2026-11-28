import js from '@eslint/js';
import svelte from 'eslint-plugin-svelte';
import globals from 'globals';
import tseslint from 'typescript-eslint';
import featureBoundary from './eslint/feature-boundary.mjs';
import featureOrder from './eslint/feature-order.mjs';
import sideEffectImport from './eslint/side-effect-import.mjs';

const tsFiles = ['**/*.{ts,tsx}'];
const scriptFiles = [
  'eslint.config.mjs',
  'eslint/**/*.mjs',
  'scripts/**/*.mjs',
  'prettier.config.js'
];
const svelteFiles = ['src/**/*.svelte', 'tests/**/*.svelte'];
const sourceFiles = ['src/**/*.{ts,svelte}'];
const testFiles = [
  'src/**/*.test.ts',
  'src/**/test/**',
  'tests/unit/**/*.test.ts',
  'tests/unit/support/**'
];
const sharedLayers = [
  '.',
  'management',
  'session',
  'editing',
  'listing',
  'components',
  'domain',
  'shell'
];
const orderedFeatures = [
  'budgets',
  'identity',
  'overview',
  'access',
  'agents',
  'providers',
  'routes',
  'inference',
  'usage',
  'guardrails',
  'exports',
  'code-mode',
  'health',
  'media',
  'notifications',
  'plugins',
  'resources',
  'settings'
];
const apiModules = [
  'src/lib/management/**',
  'src/lib/**/api.ts',
  'src/lib/features/providers/billing.ts',
  'src/lib/features/providers/catalog.ts',
  'src/lib/features/providers/credentials.ts',
  'src/lib/features/providers/grants.ts',
  'src/lib/features/providers/models.ts',
  'src/lib/features/providers/providerProfiles.ts',
  'src/lib/features/providers/providers.ts',
  'src/lib/features/providers/revisions.ts',
  'src/lib/shell/branding/branding.ts',
  'src/lib/features/usage/discounts.ts',
  'src/lib/features/usage/pricing.ts',
  'src/lib/features/usage/pricingSources.ts',
  'src/lib/features/usage/quote.ts',
  'src/lib/features/usage/rateCards.ts',
  'src/lib/features/usage/reconciliation.ts',
  'src/lib/features/usage/usage.ts'
];
const mutationHeaderLiteral = {
  selector: 'Literal[value=/^(Idempotency-Key|If-Match)$/]',
  message:
    'Build mutation headers with ifMatch, mutationHeaders or createHeaders from #lib/management/http.js.'
};
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
      'src/lib/management/schema.d.ts',
      'src/lib/management/requirements.ts'
    ]
  },
  ...svelte.configs['flat/recommended'],
  {
    ...js.configs.recommended,
    files: scriptFiles,
    languageOptions: {
      sourceType: 'module',
      globals: globals.node
    }
  },
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
      // svelte/prefer-const replaces it so `$props()` and `$derived()` may stay `let`.
      'prefer-const': 'off',
      'svelte/prefer-const': 'error',
      'svelte/no-navigation-without-resolve': ['error', { ignoreLinks: true }]
    }
  },
  {
    files: sourceFiles,
    ignores: testFiles,
    plugins: {
      console: {
        rules: {
          'feature-boundary': featureBoundary,
          'feature-order': featureOrder,
          'side-effect-import': sideEffectImport
        }
      }
    },
    rules: {
      'console/feature-boundary': ['error', { layers: sharedLayers }],
      'console/feature-order': ['error', { order: orderedFeatures }],
      'console/side-effect-import': 'error'
    }
  },
  {
    files: sourceFiles,
    ignores: [...apiModules, ...testFiles],
    rules: {
      'console/feature-boundary': [
        'error',
        { layers: sharedLayers, apiClient: 'session/client' }
      ],
      'no-restricted-globals': [
        'error',
        {
          name: 'fetch',
          message:
            'Use a feature api module or #lib/management/gateway.js gatewayFetch.'
        }
      ]
    }
  },
  {
    files: ['src/**/*.svelte'],
    rules: {
      'max-lines': [
        'error',
        { max: 400, skipBlankLines: true, skipComments: true }
      ]
    }
  },
  {
    files: tsFiles,
    ignores: [...testFiles, 'tests/**'],
    rules: {
      'max-lines': [
        'error',
        { max: 500, skipBlankLines: true, skipComments: true }
      ]
    }
  },
  {
    files: [...tsFiles, ...svelteFiles, ...scriptFiles],
    ignores: [...testFiles, 'tests/**'],
    rules: {
      'max-lines-per-function': [
        'error',
        { max: 100, skipComments: true, IIFEs: true }
      ]
    }
  },
  { files: scriptFiles, rules: { 'prefer-const': 'error' } },
  {
    files: sourceFiles,
    ignores: ['src/lib/management/**', ...testFiles],
    rules: { 'no-restricted-syntax': ['error', mutationHeaderLiteral] }
  },
  {
    files: ['src/**/*.svelte'],
    ignores: testFiles,
    rules: {
      'no-restricted-syntax': [
        'error',
        mutationHeaderLiteral,
        {
          selector: 'TryStatement',
          message:
            'Components do not catch errors. Run a write through useAction or useMutation from #lib/editing/action.svelte.js, and move other fallible logic into a .ts module.'
        }
      ]
    }
  },
  {
    files: testFiles,
    ignores: ['tests/unit/support/eventually.ts'],
    rules: {
      'no-restricted-properties': [
        'error',
        {
          object: 'vi',
          property: 'waitFor',
          message:
            'Use eventually from #test/eventually.js. vi.waitFor polls every 50ms, which dominates the console unit budget.'
        },
        {
          object: 'vi',
          property: 'waitUntil',
          message:
            'Use eventually from #test/eventually.js. vi.waitUntil polls every 50ms, which dominates the console unit budget.'
        }
      ]
    }
  }
];
