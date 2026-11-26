import { RuleTester } from 'eslint';
import tseslint from 'typescript-eslint';
import { describe, it } from 'vitest';
import sideEffectImport from '../../eslint/side-effect-import.mjs';

RuleTester.describe = describe;
RuleTester.it = it;
RuleTester.itOnly = it.only;

const dropped = [{ messageId: 'dropped' }];

new RuleTester({ languageOptions: { parser: tseslint.parser } }).run(
  'side-effect-import',
  sideEffectImport,
  {
    valid: [
      {
        code: "import '../app.css'; import '#lib/theme.css'; import 'some-polyfill'; import { a } from './setup.js'; import * as b from '#lib/x.js'; export { a, b };"
      }
    ],
    invalid: [
      { code: "import './setup.js';", errors: dropped },
      { code: "import '#lib/telemetry.js';", errors: dropped },
      { code: "import '$lib/Widget.svelte';", errors: dropped }
    ]
  }
);
