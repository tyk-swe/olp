import { instant } from '$lib/api/query';
import type { UsageFilters } from '$lib/features/usage/api/usage';
import { dateTimeLocalValue } from '$lib/format';
import { timeOrder, timeValid, UUID } from '$lib/lists/filters';

const dimensions = [
  'route',
  'provider',
  'model',
  'api_key',
  'operation',
  'attribution'
] as const;
const resources = [
  'route',
  'model',
  'provider_id',
  'api_key_id',
  'operation',
  'attribution_key',
  'attribution_value'
] as const;

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

function usageInstant(value: string, fromUrl: boolean): string | undefined {
  if (!timeValid(value, fromUrl)) return undefined;
  const normalized = instant(value);
  if (normalized === undefined) return undefined;
  const fraction = /\.(\d{4,9})(?:Z|[+-]\d{2}:\d{2})?$/i.exec(value)?.[1];
  if (fraction === undefined) return normalized;
  return `${normalized.slice(0, 19)}.${fraction}Z`;
}

export function usageProblem(state: UsageState): string | null {
  const {
    start,
    end,
    provider_id,
    api_key_id,
    attribution_key,
    attribution_value
  } = state.filters;
  if (!timeValid(start, true) || !timeValid(end, true))
    return 'Enter valid start and end times.';
  if (timeOrder(start) >= timeOrder(end)) return 'End must be after start.';
  if (provider_id && !UUID.test(provider_id))
    return 'Provider ID must be a UUID.';
  if (api_key_id && !UUID.test(api_key_id)) return 'API key ID must be a UUID.';
  if (attribution_value && !attribution_key)
    return 'An attribution value needs its attribution key.';
  if (state.dimension === 'attribution' && !attribution_key)
    return 'The attribution breakdown needs an attribution key.';
  return null;
}

export function readUsageState(
  search: URLSearchParams,
  defaults: UsageState
): UsageState {
  const start = search.get('start')?.trim() || defaults.filters.start;
  const end = search.get('end')?.trim() || defaults.filters.end;
  const filters: UsageFilters = {
    start: usageInstant(start, true) ?? start,
    end: usageInstant(end, true) ?? end
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
    attribution_key: state.filters.attribution_key ?? '',
    attribution_value: state.filters.attribution_value ?? '',
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
      timeValid(applied.filters[field], true) &&
      draft[field] === dateTimeLocalValue(applied.filters[field])
        ? applied.filters[field]
        : (usageInstant(draft[field], false) ?? draft[field]);
  }
  return state;
}
