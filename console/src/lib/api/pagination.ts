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

export function pushCursor(state: CursorHistory, next: string | null | undefined) {
  if (!next) return;
  state.history = [...state.history, state.cursor];
  state.cursor = next;
}

export function popCursor(state: CursorHistory) {
  state.cursor = state.history.at(-1);
  state.history = state.history.slice(0, -1);
}

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
