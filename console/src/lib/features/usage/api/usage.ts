import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { apiProblem, unwrap } from '$lib/api/http';
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
  project_id?: string;
  session_id?: string;
  operation?: string;
  attribution_key?: string;
  attribution_value?: string;
};

type UsageSummary = components['schemas']['UsageSummaryResponse'];
type UsageSeriesResult = components['schemas']['UsageTimeSeriesResponse'];
type UsageBreakdownResult = components['schemas']['UsageBreakdownResponse'];

export async function usageSummary(
  filters: UsageFilters
): Promise<UsageSummary> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/usage/summary',
    {
      params: { query: compactQuery(filters) }
    }
  );
  return unwrap({ data, error, response });
}

export async function usageSeries(
  filters: UsageFilters,
  granularity: 'hour' | 'day'
): Promise<UsageSeriesResult> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/usage/time-series',
    { params: { query: compactQuery({ ...filters, granularity }) } }
  );
  return unwrap({ data, error, response });
}

export type UsageDimension =
  | 'route'
  | 'provider'
  | 'model'
  | 'model_family'
  | 'estimate_provenance'
  | 'api_key'
  | 'end_user'
  | 'operation'
  | 'attribution'
  | 'project'
  | 'session';

export async function usageBreakdown(
  filters: UsageFilters,
  dimension: UsageDimension
): Promise<UsageBreakdownResult> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/usage/breakdown',
    { params: { query: compactQuery({ ...filters, dimension, limit: 50 }) } }
  );
  return unwrap({ data, error, response });
}

export async function usageCompleteness(
  filters: UsageFilters
): Promise<UsageCompleteness> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/usage/completeness',
    { params: { query: compactQuery(filters) } }
  );
  return unwrap({ data, error, response });
}

export async function exportUsageCsv(
  filters: UsageFilters,
  dimension: UsageDimension
): Promise<string> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/usage/export.csv',
    {
      params: { query: compactQuery({ ...filters, dimension }) },
      parseAs: 'text'
    }
  );
  if (!response.ok) throw apiProblem(error, response);
  return data as string;
}
