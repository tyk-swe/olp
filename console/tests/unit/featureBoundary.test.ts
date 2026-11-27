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
const layer = [{ messageId: 'layer' }];
const test = [{ messageId: 'test' }];
const routes = [{ messageId: 'routes' }];
const apiClient = [{ messageId: 'apiClient' }];
const order = ['.', 'management', 'session', 'components', 'shell'];
const ordered = [{ layers: order }];
const guarded = [{ layers: order, apiClient: 'session/client' }];

new RuleTester({ languageOptions: { parser: tseslint.parser } }).run(
  'feature-boundary',
  featureBoundary,
  {
    valid: [
      {
        filename: inLib('features/exports/SinkEditor.svelte.ts'),
        code: "import { a } from '#lib/features/exports/api.js'; import { b } from './parts/form.js'; import { c } from '#lib/features/routes/index.js'; import { d } from '../routes/index.js'; import { e } from '#lib/components/x.js';"
      },
      {
        filename: inLib('features/exports/parts/form.ts'),
        code: "import { a } from '../api.js'; export { b } from '../../routes/index.js';"
      },
      {
        filename: path.resolve('src/routes/exports/+page.ts'),
        code: "import { a } from '#lib/features/exports/api.js';"
      },
      {
        filename: inLib('shell/Sidebar.ts'),
        options: ordered,
        code: "import { a } from '#lib/components/Menu.js'; import { b } from '../management/http.js'; import { c } from './navigation.js'; export { d } from '../format.js'; import { e } from 'svelte';"
      },
      {
        filename: inLib('features/exports/api.ts'),
        options: ordered,
        code: "import { a } from '#lib/shell/navigation.js'; import { b } from '#lib/management/http.js';"
      },
      {
        filename: inLib('unlisted/widget.ts'),
        options: ordered,
        code: "import { a } from '#lib/shell/navigation.js';"
      },
      {
        filename: inLib('features/exports/api.ts'),
        options: ordered,
        code: "export type A = import('#lib/management/http.js').CursorPage<string>; export type B = import('./form.js').Form; export type C = import('#lib/features/routes/index.js').Route;"
      },
      {
        filename: path.resolve('src/routes/exports/+page.ts'),
        options: guarded,
        code: "import { a } from './form.js'; import { b } from '#lib/features/exports/latest.js';"
      },
      {
        filename: inLib('session/api.ts'),
        options: ordered,
        code: "import { apiClient } from './client.js'; export { apiClient };"
      }
    ],
    invalid: [
      {
        filename: inLib('features/exports/api.ts'),
        code: "import { a } from '#lib/features/routes/routeEditor.js';",
        errors: entry
      },
      {
        filename: inLib('features/exports/api.ts'),
        code: "export { a } from '../routes/routeEditor.js'; export * from '../routes/parts/index.js';",
        errors: [...entry, ...entry]
      },
      {
        filename: inLib('features/exports/parts/form.ts'),
        code: "const a = () => import('../../routes/routeEditor.js'); export { a };",
        errors: entry
      },
      {
        filename: inLib('components/Picker.ts'),
        code: "import { a } from '#lib/features/exports/index.js'; import { b } from '../features/exports/api.js';",
        errors: [...shared, ...shared]
      },
      {
        filename: inLib('format.ts'),
        code: "import type { A } from './features/exports/index.js'; export type { A };",
        errors: shared
      },
      {
        filename: inLib('components/Tabs.ts'),
        options: ordered,
        code: "import { a } from '#lib/shell/navigation.js'; export { b } from '../shell/indicator.js'; export * from '../shell/branding/branding.js'; const c = () => import('#lib/shell/branding/branding.js'); export { a, c };",
        errors: [...layer, ...layer, ...layer, ...layer]
      },
      {
        filename: inLib('format.ts'),
        options: ordered,
        code: "import { a } from './management/http.js'; export { a };",
        errors: layer
      },
      {
        filename: inLib('shell/Sidebar.ts'),
        options: ordered,
        code: "import { a } from '#lib/unlisted/widget.js'; export { a };",
        errors: layer
      },
      {
        filename: inLib('components/Tabs.ts'),
        options: ordered,
        code: "export type A = import('#lib/shell/navigation.js').NavigationLeaf; export type B = import('$lib/shell/navigation.js').NavigationLeaf;",
        errors: [...layer, ...layer]
      },
      {
        filename: inLib('features/exports/api.ts'),
        code: "export type A = import('#lib/features/routes/routeEditor.js').Route;",
        errors: entry
      },
      {
        filename: inLib('format.ts'),
        code: "export type A = import('#lib/features/exports/index.js').Sink;",
        errors: shared
      },
      {
        filename: inLib('features/exports/api.ts'),
        options: ordered,
        code: "import { a } from '#lib/session/test/transport.js'; import { b } from './test/fixture.js'; import { c } from './api.test.js'; import { d } from '../../../../tests/unit/support/eventually.js'; import { e } from '#test/eventually.js'; export { a, b, c, d, e };",
        errors: [...test, ...test, ...test, ...test, ...test]
      },
      {
        filename: path.resolve('src/routes/exports/+page.ts'),
        options: ordered,
        code: "import { a } from '#lib/session/test/transport.js'; export { a };",
        errors: test
      },
      {
        filename: inLib('shell/Sidebar.ts'),
        options: ordered,
        code: "import { a } from '../../routes/exports/+page.js'; export { a };",
        errors: routes
      },
      {
        filename: inLib('components/Picker.ts'),
        options: guarded,
        code: "import { a } from '#lib/session/client.js'; import { b } from '../session/client.js'; import { c } from '../session/client'; export { a, b, c };",
        errors: [...apiClient, ...apiClient, ...apiClient]
      },
      {
        filename: inLib('session/lifecycle.ts'),
        options: guarded,
        code: "import { apiClient } from './client.js'; export { apiClient };",
        errors: apiClient
      },
      {
        filename: path.resolve('src/routes/exports/+page.ts'),
        options: guarded,
        code: "import { apiClient } from '#lib/session/client.js'; export { apiClient };",
        errors: apiClient
      }
    ]
  }
);
