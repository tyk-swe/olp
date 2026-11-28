import path from 'node:path';
import { RuleTester } from 'eslint';
import tseslint from 'typescript-eslint';
import { describe, it } from 'vitest';
import featureOrder from '../../eslint/feature-order.mjs';

RuleTester.describe = describe;
RuleTester.it = it;
RuleTester.itOnly = it.only;

const inLib = (file: string) => path.resolve('src/lib', file);
const options = [{ order: ['providers', 'routes', 'usage'] }];
const later = [{ messageId: 'later' }];
const unlisted = [{ messageId: 'unlisted' }];

new RuleTester({ languageOptions: { parser: tseslint.parser } }).run(
  'feature-order',
  featureOrder,
  {
    valid: [
      {
        filename: inLib('features/routes/api.ts'),
        options,
        code: "import { a } from '#lib/features/providers/index.js'; import { b } from './form.js'; import { c } from '#lib/features/routes/index.js'; import { d } from '#lib/components/x.js'; import { e } from 'svelte';"
      },
      {
        filename: inLib('shell/navigation.ts'),
        options,
        code: "import { a } from '#lib/features/usage/index.js';"
      }
    ],
    invalid: [
      {
        filename: inLib('features/providers/api.ts'),
        options,
        code: "import { a } from '#lib/features/routes/index.js'; import type { B } from '$lib/features/usage/index.js';",
        errors: [...later, ...later]
      },
      {
        filename: inLib('features/providers/parts/form.ts'),
        options,
        code: "export { a } from '../../routes/index.js'; export * from '../../routes'; export type C = import('#lib/features/usage/index.js').C; export const d = () => import('#lib/features/usage/index.js');",
        errors: [...later, ...later, ...later, ...later]
      },
      {
        filename: inLib('features/exports/api.ts'),
        options,
        code: "import { a } from '#lib/features/providers/index.js';",
        errors: unlisted
      },
      {
        filename: inLib('features/usage/api.ts'),
        options,
        code: "import { a } from '#lib/features/exports/index.js';",
        errors: unlisted
      }
    ]
  }
);
