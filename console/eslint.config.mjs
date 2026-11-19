import { readdirSync, readFileSync } from 'node:fs';
import js from '@eslint/js';
import svelte from 'eslint-plugin-svelte';
import globals from 'globals';
import tseslint from 'typescript-eslint';

const tsFiles = ['**/*.{ts,tsx}'];
const svelteFiles = ['src/**/*.svelte', 'tests/**/*.svelte'];
const sourceFiles = ['src/**/*.{ts,svelte}'];
const testFiles = ['src/**/*.test.ts', 'src/**/test/**'];
const sharedLayers = [
  'src/lib/*.ts',
  'src/lib/{components,editing,listing,management,session,shell}/**/*.{ts,svelte}'
];
const apiModules = [
  'src/lib/management/**',
  'src/lib/features/**/api.ts',
  'src/lib/session/api.ts',
  'src/lib/features/providers/billing.ts',
  'src/lib/features/providers/catalog.ts',
  'src/lib/features/providers/credentials.ts',
  'src/lib/features/providers/grants.ts',
  'src/lib/features/providers/models.ts',
  'src/lib/features/providers/providerProfiles.ts',
  'src/lib/features/providers/providers.ts',
  'src/lib/features/providers/revisions.ts',
  'src/lib/features/resources/retrieval.ts',
  'src/lib/shell/branding/branding.ts',
  'src/lib/features/usage/discounts.ts',
  'src/lib/features/usage/pricing.ts',
  'src/lib/features/usage/pricingSources.ts',
  'src/lib/features/usage/quote.ts',
  'src/lib/features/usage/rateCards.ts',
  'src/lib/features/usage/reconciliation.ts',
  'src/lib/features/usage/usage.ts'
];
const legacy = JSON.parse(
  readFileSync(new URL('./eslint-legacy.json', import.meta.url), 'utf8')
);
const featureNames = readdirSync(
  new URL('./src/lib/features', import.meta.url),
  { withFileTypes: true }
)
  .filter((entry) => entry.isDirectory())
  .map((entry) => entry.name);

const apiClientOnly = {
  name: '#lib/management/client.js',
  message:
    'Call the management API from the feature api module, not a component.'
};
const noFeatures = {
  group: ['\\#lib/features/**'],
  message:
    'Shared layers must not import a feature. Move the shared part into a shared layer or pass it in.'
};
const featureEntryOnly = (feature) => ({
  group: [
    '\\#lib/features/*/**',
    `!\\#lib/features/${feature}/**`,
    '!\\#lib/features/*/index.js'
  ],
  message: 'Import another feature only through its index.ts.'
});
const restrictedImports = (paths, patterns) => ({
  'no-restricted-imports': ['error', { paths, patterns }]
});
const inBoth = (left, right) => left.flatMap((a) => right.map((b) => [a, b]));
// A later block replaces the whole no-restricted-imports option of an earlier
// one, so each boundary restates the api client restriction it must keep.
const importBoundary = (files, legacyFiles, pattern) => [
  {
    files,
    ignores: [...apiModules, ...testFiles, ...legacyFiles],
    rules: restrictedImports([apiClientOnly], [pattern])
  },
  {
    files: inBoth(apiModules, files),
    ignores: [...testFiles, ...legacyFiles],
    rules: restrictedImports([], [pattern])
  }
];
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
      'src/lib/management/schema.d.ts',
      'src/lib/management/requirements.ts'
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
    // Feature api modules (and shared transport) own every apiClient call;
    // browser code reaches the inference gateway only through gatewayFetch.
    files: sourceFiles,
    ignores: [...apiModules, ...testFiles],
    rules: {
      ...restrictedImports([apiClientOnly], []),
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
  ...importBoundary(sharedLayers, legacy.sharedImportsFeature, noFeatures),
  ...featureNames.flatMap((feature) =>
    importBoundary(
      [`src/lib/features/${feature}/**/*.{ts,svelte}`],
      legacy.crossFeatureDeepImport,
      featureEntryOnly(feature)
    )
  ),
  {
    files: ['src/**/*.svelte'],
    ignores: legacy.componentOverMaxLines,
    rules: {
      'max-lines': [
        'error',
        { max: 400, skipBlankLines: true, skipComments: true }
      ]
    }
  },
  {
    files: sourceFiles,
    ignores: [
      'src/lib/management/**',
      ...testFiles,
      ...legacy.rawMutationHeader
    ],
    rules: {
      'no-restricted-syntax': [
        'error',
        {
          selector: 'Literal[value=/^(Idempotency-Key|If-Match)$/]',
          message:
            'Build mutation headers with mutationHeaders or createHeaders from #lib/management/http.js.'
        }
      ]
    }
  }
];
