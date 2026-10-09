export type FilterScope = 'usage' | 'history';
export type SavedFilter = { name: string; search: string };
const prefix = 'olp.saved-filters.v1.';
const fields: Record<FilterScope, string[]> = {
  usage: [
    'start',
    'end',
    'dimension',
    'granularity',
    'route',
    'model',
    'provider_id',
    'api_key_id',
    'operation',
    'attribution_key',
    'attribution_value'
  ],
  history: [
    'route',
    'provider_id',
    'model',
    'api_key_id',
    'operation',
    'status_code',
    'error_class',
    'started_after',
    'started_before'
  ]
};

export function filterSearch(scope: FilterScope, search: string): string {
  const source = new URLSearchParams(search);
  const target = new URLSearchParams();
  for (const field of fields[scope]) {
    const value = source.get(field);
    if (value) target.set(field, value);
  }
  const result = target.toString();
  return result ? `?${result}` : '';
}

function key(session: string, scope: FilterScope): string {
  if (
    !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(
      session
    )
  )
    throw new Error('A verified session is required to save filters.');
  return `${prefix}${session}.${scope}`;
}

export function readSavedFilters(
  storage: Storage,
  session: string,
  scope: FilterScope
): SavedFilter[] {
  // Retire views from earlier sessions in this tab, including a different user.
  for (let index = storage.length - 1; index >= 0; index--) {
    const storedKey = storage.key(index);
    if (
      storedKey?.startsWith(prefix) &&
      !storedKey.startsWith(`${prefix}${session}.`)
    )
      storage.removeItem(storedKey);
  }
  try {
    const value: unknown = JSON.parse(
      storage.getItem(key(session, scope)) || '[]'
    );
    if (!Array.isArray(value)) return [];
    const names = new Set<string>();
    return value.slice(0, 20).flatMap((entry) => {
      if (
        !entry ||
        typeof entry.name !== 'string' ||
        typeof entry.search !== 'string' ||
        !entry.name.trim() ||
        entry.name.length > 60 ||
        entry.search.length > 4096
      )
        return [];
      const name = entry.name.trim();
      if (names.has(name)) return [];
      names.add(name);
      return [{ name, search: filterSearch(scope, entry.search) }];
    });
  } catch {
    return [];
  }
}

export function saveFilters(
  storage: Storage,
  session: string,
  scope: FilterScope,
  filters: SavedFilter[]
) {
  if (filters.length > 20)
    throw new Error('Keep at most 20 saved views per page.');
  const normalized = filters.map(({ name, search }) => {
    if (!name.trim() || name.length > 60 || search.length > 4096)
      throw new Error(
        'Use a view name of 1–60 characters and a bounded filter query.'
      );
    return { name: name.trim(), search: filterSearch(scope, search) };
  });
  storage.setItem(key(session, scope), JSON.stringify(normalized));
}
