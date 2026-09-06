import { instant } from '$lib/api/query';
import type { UsageFilters } from '$lib/api/usage';
import { dateTimeLocalValue } from '$lib/format';

const dimensions = [
  'route',
  'provider',
  'model',
  'api_key',
  'operation'
] as const;
const resources = [
  'route',
  'model',
  'provider_id',
  'api_key_id',
  'operation'
] as const;
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export type UsageState = {
  filters: UsageFilters;
  dimension: (typeof dimensions)[number];
  granularity: 'hour' | 'day';
};

export type UsageDraft = Required<UsageFilters> &
  Pick<UsageState, 'dimension' | 'granularity'>;

export function defaultUsageState(now = new Date()): UsageState {
  return {
    filters: {
      start: new Date(now.valueOf() - 24 * 60 * 60 * 1000).toISOString(),
      end: now.toISOString()
    },
    dimension: 'route',
    granularity: 'hour'
  };
}

function urlInstant(value: string): string | undefined {
  return /T.*(?:Z|[+-]\d{2}:\d{2})$/i.test(value) ? instant(value) : undefined;
}

export function usageProblem(state: UsageState): string | null {
  const { start, end, provider_id, api_key_id } = state.filters;
  if (!urlInstant(start) || !urlInstant(end))
    return 'Enter valid start and end times.';
  if (new Date(start) >= new Date(end)) return 'End must be after start.';
  if (provider_id && !uuid.test(provider_id))
    return 'Provider ID must be a UUID.';
  if (api_key_id && !uuid.test(api_key_id)) return 'API key ID must be a UUID.';
  return null;
}

export function readUsageState(
  search: URLSearchParams,
  defaults: UsageState
): UsageState {
  const start = search.get('start')?.trim() || defaults.filters.start;
  const end = search.get('end')?.trim() || defaults.filters.end;
  const filters: UsageFilters = {
    start: urlInstant(start) ?? start,
    end: urlInstant(end) ?? end
  };
  for (const field of resources) {
    const value = search.get(field)?.trim();
    if (value)
      filters[field] = field.endsWith('_id') ? value.toLowerCase() : value;
  }
  return {
    filters,
    dimension:
      dimensions.find((value) => value === search.get('dimension')) ?? 'route',
    granularity: search.get('granularity') === 'day' ? 'day' : 'hour'
  };
}

export function usageSearch(state: UsageState): string {
  const search = new URLSearchParams({
    start: state.filters.start,
    end: state.filters.end
  });
  for (const field of resources) {
    const value = state.filters[field];
    if (value) search.set(field, value);
  }
  search.set('dimension', state.dimension);
  search.set('granularity', state.granularity);
  return search.toString();
}

export function usageDraft(state: UsageState): UsageDraft {
  return {
    start: dateTimeLocalValue(state.filters.start),
    end: dateTimeLocalValue(state.filters.end),
    route: state.filters.route ?? '',
    model: state.filters.model ?? '',
    provider_id: state.filters.provider_id ?? '',
    api_key_id: state.filters.api_key_id ?? '',
    operation: state.filters.operation ?? '',
    dimension: state.dimension,
    granularity: state.granularity
  };
}

export function applyUsageDraft(
  draft: UsageDraft,
  applied: UsageState
): UsageState {
  const state = readUsageState(new URLSearchParams(draft), applied);
  for (const field of ['start', 'end'] as const) {
    state.filters[field] =
      draft[field] === dateTimeLocalValue(applied.filters[field])
        ? applied.filters[field]
        : (instant(draft[field]) ?? draft[field]);
  }
  return state;
}
