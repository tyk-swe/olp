import { ApiProblem, throwApiProblem } from '../http';

export type CursorPage<T> = { items: T[]; nextCursor: string | null };
export type ReadSignal = AbortSignal | { readonly signal: AbortSignal };

export function getAbortSignal(input?: ReadSignal): AbortSignal | undefined {
  if (input && 'signal' in input) return input.signal;
  return input;
}

export function result<T>(data: T | undefined, error: unknown, response: Response): T {
  if (data === undefined) throwApiProblem(error, response);
  return data;
}

export async function collectCursorPages<T>(
  load: (cursor?: string) => Promise<CursorPage<T>>
): Promise<T[]> {
  const items: T[] = [];
  const seen = new Set<string>();
  let cursor: string | undefined;
  do {
    const page = await load(cursor);
    items.push(...page.items);
    if (items.length > 10_000) {
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
