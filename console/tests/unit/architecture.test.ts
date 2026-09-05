import { ESLint } from 'eslint';
import { describe, expect, it } from 'vitest';

const lint = new ESLint();

async function boundaryMessages(file: string, source: string) {
  const [result] = await lint.lintText(source, {
    filePath: `src/lib/${file}`
  });
  return result.messages.filter(
    (message) => message.ruleId === 'olp/no-cross-feature-imports'
  );
}

describe('architecture boundaries', () => {
  it.each([
    ['shared.ts', "import '$lib/features/gateway/example';", 'neutral'],
    ['lists/shared.ts', "import '../features/gateway/example';", 'neutral'],
    ['shared.ts', "void import('$lib/features/gateway/example');", 'neutral'],
    ['api/example.ts', "import { createContext } from 'svelte';", 'apiUi'],
    ['api/example.ts', "import '../lists/pagination';", 'apiUi'],
    [
      'api/example.ts',
      "export type { CursorPage } from './http';",
      'forwarding'
    ],
    [
      'api/example.ts',
      "import type { CursorPage } from './http'; export type { CursorPage };",
      'forwarding'
    ],
    [
      'features/gateway/example.ts',
      "import '$lib/features/access/example';",
      'crossFeature'
    ]
  ])('rejects %s: %s', async (file, source, expected) => {
    expect(
      (await boundaryMessages(file, source)).map((message) => message.messageId)
    ).toContain(expected);
  });

  it.each([
    ['features/gateway/example.ts', "import '$lib/features/gateway/shared';"],
    ['features/gateway/example.ts', "import '$lib/api/http';"],
    ['api/example.ts', 'export type Page = { nextCursor?: string };'],
    ['lists/example.ts', "import { createContext } from 'svelte';"]
  ])('allows %s: %s', async (file, source) => {
    expect(await boundaryMessages(file, source)).toEqual([]);
  });
});
