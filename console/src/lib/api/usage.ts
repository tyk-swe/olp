import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { result } from '$lib/api/http';
import { compactQuery } from '$lib/api/query';

export type UsagePoint = components['schemas']['UsagePointResponse'];
export type UsageCompleteness =
  components['schemas']['UsageCompletenessResponse'];

export type UsageFilters = {
  start: string;
  end: string;
  route?: string;
  provider_id?: string;
  model?: string;
  api_key_id?: string;
  operation?: string;
};

type UsageSummary = components['schemas']['UsageSummaryResponse'];
type UsageSeriesResult = components['schemas']['UsageTimeSeriesResponse'];
type UsageBreakdownResult = components['schemas']['UsageBreakdownResponse'];

export async function usageSummary(
  filters: UsageFilters
): Promise<UsageSummary> {
  const { data, error, response } = await apiClient.GET(
    '/api/v3/usage/summary',
    {
      params: { query: compactQuery(filters) }
    }
  );
  return result(data, error, response);
}

export async function usageSeries(
  filters: UsageFilters,
  granularity: 'hour' | 'day'
): Promise<UsageSeriesResult> {
  const { data, error, response } = await apiClient.GET(
    '/api/v3/usage/time-series',
    { params: { query: compactQuery({ ...filters, granularity }) } }
  );
  return result(data, error, response);
}

export async function usageBreakdown(
  filters: UsageFilters,
  dimension: 'route' | 'provider' | 'model' | 'api_key' | 'operation'
): Promise<UsageBreakdownResult> {
  const { data, error, response } = await apiClient.GET(
    '/api/v3/usage/breakdown',
    { params: { query: compactQuery({ ...filters, dimension, limit: 50 }) } }
  );
  return result(data, error, response);
}

export async function usageCompleteness(
  filters: UsageFilters
): Promise<UsageCompleteness> {
  const { data, error, response } = await apiClient.GET(
    '/api/v3/usage/completeness',
    { params: { query: compactQuery(filters) } }
  );
  return result(data, error, response);
}
