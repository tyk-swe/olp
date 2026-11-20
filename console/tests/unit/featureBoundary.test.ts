import path from 'node:path';
import { RuleTester } from 'eslint';
import tseslint from 'typescript-eslint';
import { describe, it } from 'vitest';
import featureBoundary from '../../eslint/feature-boundary.mjs';

RuleTester.describe = describe;
RuleTester.it = it;
RuleTester.itOnly = it.only;

const inLib = (file: string) => path.resolve('src/lib', file);
const entry = [{ messageId: 'entry' }];
const shared = [{ messageId: 'shared' }];

new RuleTester({ languageOptions: { parser: tseslint.parser } }).run(
  'feature-boundary',
  featureBoundary,
  {
    valid: [
      {
        filename: inLib('features/sinks/SinkEditor.svelte.ts'),
        code: "import { a } from '#lib/features/sinks/api.js'; import { b } from './parts/form.js'; import { c } from '#lib/features/routes/index.js'; import { d } from '../routes/index.js'; import { e } from '#lib/components/x.js';"
      },
      {
        filename: inLib('features/sinks/parts/form.ts'),
        code: "import { a } from '../api.js'; export { b } from '../../routes/index.js';"
      },
      {
        filename: path.resolve('src/routes/sinks/+page.ts'),
        code: "import { a } from '#lib/features/sinks/api.js';"
      }
    ],
    invalid: [
      {
        filename: inLib('features/sinks/api.ts'),
        code: "import { a } from '#lib/features/routes/routeEditor.js';",
        errors: entry
      },
      {
        filename: inLib('features/sinks/api.ts'),
        code: "export { a } from '../routes/routeEditor.js'; export * from '../routes/parts/index.js';",
        errors: [...entry, ...entry]
      },
      {
        filename: inLib('features/sinks/parts/form.ts'),
        code: "const a = () => import('../../routes/routeEditor.js'); export { a };",
        errors: entry
      },
      {
        filename: inLib('components/Picker.ts'),
        code: "import { a } from '#lib/features/sinks/index.js'; import { b } from '../features/sinks/api.js';",
        errors: [...shared, ...shared]
      },
      {
        filename: inLib('format.ts'),
        code: "import type { A } from './features/sinks/index.js'; export type { A };",
        errors: shared
      }
    ]
  }
);
