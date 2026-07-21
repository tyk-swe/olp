import { ApiProblem } from './http';

export type CursorPage<T> = { items: T[]; nextCursor: string | null };

const MAX_COLLECTED_ITEMS = 10_000;

export async function collectCursorPages<T>(
  load: (cursor?: string) => Promise<CursorPage<T>>
): Promise<T[]> {
  const items: T[] = [];
  const seen = new Set<string>();
  let cursor: string | undefined;
  do {
    const page = await load(cursor);
    items.push(...page.items);
    if (items.length > MAX_COLLECTED_ITEMS) {
      throw new ApiProblem({
        type: 'urn:olp:problem:pagination-limit-exceeded',
        title: 'The control API collection exceeds the console safety limit',
        status: 502
      });
    }
    const next = page.nextCursor ?? undefined;
    if (!next) break;
    if (seen.has(next)) {
      throw new ApiProblem({
        type: 'urn:olp:problem:invalid-cursor-cycle',
        title: 'The control API returned a repeated pagination cursor',
        status: 502
      });
    }
    seen.add(next);
    cursor = next;
  } while (cursor);
  return items;
}
