import { ApiProblem } from './http';

export type CursorPage<T> = { items: T[]; nextCursor: string | null };

/// Back/forward cursor pagination state shared by the list pages. `history`
/// holds the cursor of every previous page (undefined = first page) so the
/// "previous" control can pop back without a server round-trip.
export type CursorHistory = {
  cursor: string | undefined;
  history: Array<string | undefined>;
};

export function emptyCursorHistory(): CursorHistory {
  return { cursor: undefined, history: [] };
}

export function pushCursor(state: CursorHistory, next: string | undefined) {
  state.history = [...state.history, state.cursor];
  state.cursor = next;
}

export function popCursor(state: CursorHistory) {
  state.cursor = state.history.at(-1);
  state.history = state.history.slice(0, -1);
}

const MAX_COLLECTED_ITEMS = 10_000;

export async function collectCursorPages<T>(
  load: (cursor: string | undefined, remaining: number) => Promise<CursorPage<T>>
): Promise<T[]> {
  const items: T[] = [];
  const seen = new Set<string>();
  let cursor: string | undefined;
  do {
    const remaining = MAX_COLLECTED_ITEMS - items.length;
    const page = await load(cursor, remaining);
    const next = page.nextCursor ?? undefined;
    if (page.items.length > remaining || (next && page.items.length === remaining)) {
      throw new ApiProblem({
        type: 'urn:olp:problem:pagination-limit-exceeded',
        title: 'The control API collection exceeds the console safety limit',
        status: 502
      });
    }
    items.push(...page.items);
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
