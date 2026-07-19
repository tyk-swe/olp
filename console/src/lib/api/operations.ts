import type { components } from './schema';
import { apiClient } from './client';
import { ApiProblem } from './http';

export type RequestSummary = components['schemas']['RequestSummary'];
export type RequestDetail = components['schemas']['RequestDetailResponse'];
export type AuditEvent = components['schemas']['AuditEventResponse'];
export type RuntimeGeneration = components['schemas']['RuntimeGenerationItem'];
export type MediaJob = components['schemas']['MediaJobItem'];
export type Setting = components['schemas']['SettingResponse'];
export type PricingRevision = components['schemas']['PricingRevisionResponse'];
export type UsagePoint = components['schemas']['UsagePointResponse'];
export type UsageBreakdownItem = components['schemas']['UsageBreakdownItem'];
export type UsageGatewayEpoch = components['schemas']['UsageGatewayEpochResponse'];
export type UsageEpochAcknowledgement = components['schemas']['UsageEpochAcknowledgementResponse'];
export type Session = components['schemas']['SessionDetailResponse'];
export type UserProfile = components['schemas']['UserDetailResponse'];
export type OidcIdentityList = components['schemas']['OidcIdentityListResponse'];
export type Readiness = components['schemas']['HealthResponse'];

export type CursorPage<T> = { data: T[]; next_cursor?: string | null };

export type RequestFilters = {
  cursor?: string;
  limit?: number;
  route?: string;
  provider_id?: string;
  model?: string;
  api_key_id?: string;
  operation?: string;
  status_code?: number;
  error_class?: string;
  started_after?: string;
  started_before?: string;
};

export type MediaJobFilters = {
  cursor?: string;
  limit?: number;
  api_key_id?: string;
  provider_id?: string;
  route?: string;
  state?: string;
  lifecycle?: string;
  created_after?: string;
  created_before?: string;
};

export type UsageFilters = {
  start: string;
  end: string;
  route?: string;
  provider_id?: string;
  model?: string;
  api_key_id?: string;
  operation?: string;
};

export type UsageGatewayEpochState =
  | 'open'
  | 'gracefully_closed'
  | 'unresolved'
  | 'acknowledged';

type UsageRangeCoverage = {
  range_complete: boolean;
  approximate: boolean;
  excluded_partial_aggregate_boundaries: number;
};

type UsageConsumerStatus = {
  state: 'unknown' | 'healthy' | 'backlogged' | 'stale';
  pending_events: number;
  lag_events: number;
  oldest_pending_at?: string | null;
  checked_at?: string | null;
  heartbeat_age_seconds?: number | null;
};

type UsageVisibility = {
  coverage: UsageRangeCoverage;
  consumer: UsageConsumerStatus;
  complete: boolean;
};

type UsageSummary = components['schemas']['UsageSummaryResponse'] & UsageVisibility;
type UsageCompleteness = components['schemas']['UsageCompletenessResponse'] & UsageVisibility;
type UsageSeriesResult = { data: UsagePoint[]; coverage: UsageRangeCoverage };
type UsageBreakdownResult = {
  data: UsageBreakdownItem[];
  coverage: UsageRangeCoverage;
};

export type ProviderHealth = components['schemas']['ProviderHealthItem'];

export type PlaygroundRequest = Omit<components['schemas']['PlaygroundRequest'], 'surface'> & {
  surface?: 'open_ai' | 'anthropic' | 'gemini';
};
export type PlaygroundResponse = components['schemas']['PlaygroundResponse'];

export type PriceDraft = components['schemas']['PriceRequest'];

export type ProfileUpdate = { display_name: string };
export type PasswordChange = { current_password: string; new_password: string };
export type PasswordEnrollment = { new_password: string };

function compact<T extends Record<string, unknown>>(value: T): T {
  return Object.fromEntries(
    Object.entries(value).filter(([, item]) => item !== '' && item !== undefined && item !== null)
  ) as T;
}

function apiError(error: unknown, status = 500): ApiProblem {
  if (error && typeof error === 'object') {
    const value = error as Record<string, unknown>;
    return new ApiProblem({
      title: typeof value.title === 'string' ? value.title : 'Request failed',
      detail: typeof value.detail === 'string' ? value.detail : undefined,
      status: typeof value.status === 'number' ? value.status : status
    });
  }
  return new ApiProblem({ title: 'Request failed', status });
}

export async function listRequests(filters: RequestFilters): Promise<CursorPage<RequestSummary>> {
  const { data, error, response } = await apiClient.GET('/api/v1/requests', {
    params: { query: compact(filters) }
  });
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function getRequest(requestId: string): Promise<RequestDetail> {
  const { data, error, response } = await apiClient.GET('/api/v1/requests/{request_id}', {
    params: { path: { request_id: requestId } }
  });
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function listMediaJobs(filters: MediaJobFilters): Promise<CursorPage<MediaJob>> {
  const { data, error, response } = await apiClient.GET('/api/v1/media-jobs', {
    params: { query: compact(filters) }
  });
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function getMediaJob(jobId: string): Promise<MediaJob> {
  const { data, error, response } = await apiClient.GET('/api/v1/media-jobs/{job_id}', {
    params: { path: { job_id: jobId } }
  });
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function usageSummary(filters: UsageFilters): Promise<UsageSummary> {
  const { data, error, response } = await apiClient.GET('/api/v1/usage/summary', {
    params: { query: compact(filters) }
  });
  if (!data) throw apiError(error, response.status);
  return data as UsageSummary;
}

export async function listUsageGatewayEpochs(
  state: UsageGatewayEpochState,
  cursor?: string
): Promise<CursorPage<UsageGatewayEpoch>> {
  const { data, error, response } = await apiClient.GET('/api/v1/usage/gateway-epochs', {
    params: { query: { state, cursor, limit: 25 } }
  });
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function acknowledgeUsageGatewayEpoch(
  processEpoch: string
): Promise<UsageEpochAcknowledgement> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/usage/gateway-epochs/{process_epoch}/acknowledge',
    { params: { path: { process_epoch: processEpoch } } }
  );
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function usageSeries(
  filters: UsageFilters,
  granularity: 'hour' | 'day'
): Promise<UsageSeriesResult> {
  const { data, error, response } = await apiClient.GET('/api/v1/usage/time-series', {
    params: { query: compact({ ...filters, granularity }) }
  });
  if (!data) throw apiError(error, response.status);
  return data as UsageSeriesResult;
}

export async function usageBreakdown(
  filters: UsageFilters,
  dimension: 'route' | 'provider' | 'model' | 'api_key' | 'operation'
): Promise<UsageBreakdownResult> {
  const { data, error, response } = await apiClient.GET('/api/v1/usage/breakdown', {
    params: { query: compact({ ...filters, dimension, limit: 50 }) }
  });
  if (!data) throw apiError(error, response.status);
  return data as UsageBreakdownResult;
}

export async function usageCompleteness(filters: UsageFilters): Promise<UsageCompleteness> {
  const { data, error, response } = await apiClient.GET('/api/v1/usage/completeness', {
    params: { query: compact(filters) }
  });
  if (!data) throw apiError(error, response.status);
  return data as UsageCompleteness;
}

export async function listAudit(cursor?: string): Promise<CursorPage<AuditEvent>> {
  const { data, error, response } = await apiClient.GET('/api/v1/audit', {
    params: { query: compact({ cursor, limit: 50 }) }
  });
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function getReadiness(): Promise<Readiness> {
  const { data, error, response } = await apiClient.GET('/api/v1/health/ready');
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function listProviderHealth(windowMinutes = 15): Promise<{
  window_minutes: number;
  data: ProviderHealth[];
}> {
  const providers: ProviderHealth[] = [];
  const seen = new Set<string>();
  let cursor: string | undefined;
  let responseWindow = windowMinutes;
  do {
    const { data, error, response } = await apiClient.GET('/api/v1/provider-health', {
      params: { query: { window_minutes: windowMinutes, cursor, limit: 200 } }
    });
    if (!data) throw apiError(error, response.status);
    responseWindow = data.window_minutes;
    providers.push(...data.data);
    const next = data.next_cursor ?? undefined;
    if (next && seen.has(next)) {
      throw new ApiProblem({ title: 'Provider-health pagination repeated a cursor', status: 502 });
    }
    if (next) seen.add(next);
    cursor = next;
  } while (cursor);
  return { window_minutes: responseWindow, data: providers };
}

export async function listRuntimeGenerations(cursor?: string): Promise<CursorPage<RuntimeGeneration>> {
  const { data, error, response } = await apiClient.GET('/api/v1/runtime-generations', {
    params: { query: compact({ cursor, limit: 25 }) }
  });
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function listSettings(): Promise<Setting[]> {
  const { data, error, response } = await apiClient.GET('/api/v1/settings');
  if (!data) throw apiError(error, response.status);
  return data.data;
}

export async function updateSetting(setting: Setting, value: string): Promise<Setting> {
  const { data, error, response } = await apiClient.PUT('/api/v1/settings/{key}', {
    params: {
      path: { key: setting.key },
      header: { 'If-Match': `"${setting.etag}"` }
    },
    body: { value }
  });
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function listPricing(cursor?: string): Promise<CursorPage<PricingRevision>> {
  const { data, error, response } = await apiClient.GET('/api/v1/pricing/revisions', {
    params: { query: compact({ cursor, limit: 25 }) }
  });
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function createPricingRevision(
  effectiveAt: string,
  prices: PriceDraft[]
): Promise<PricingRevision> {
  const { data, error, response } = await apiClient.POST('/api/v1/pricing/revisions', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    body: { effective_at: effectiveAt, prices }
  });
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function getProfile(): Promise<UserProfile> {
  const { data, error, response } = await apiClient.GET('/api/v1/profile');
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function updateProfile(profile: UserProfile, input: ProfileUpdate): Promise<UserProfile> {
  const { data, error, response } = await apiClient.PATCH('/api/v1/profile', {
    params: { header: { 'If-Match': `"${profile.etag}"` } },
    body: input
  });
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function changePassword(profile: UserProfile, input: PasswordChange): Promise<UserProfile> {
  const { data, error, response } = await apiClient.POST('/api/v1/profile/password', {
    params: { header: { 'If-Match': `"${profile.etag}"` } },
    body: input
  });
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function enrollPassword(
  profile: UserProfile,
  input: PasswordEnrollment
): Promise<UserProfile> {
  const { data, error, response } = await apiClient.POST('/api/v1/profile/password/enroll', {
    params: { header: { 'If-Match': `"${profile.etag}"` } },
    body: input
  });
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function listSessions(cursor?: string): Promise<CursorPage<Session>> {
  const { data, error, response } = await apiClient.GET('/api/v1/sessions', {
    params: { query: compact({ cursor, limit: 50 }) }
  });
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function revokeSession(id: string): Promise<void> {
  const { error, response } = await apiClient.DELETE('/api/v1/sessions/{session_id}', {
    params: { path: { session_id: id } }
  });
  if (!response.ok) throw apiError(error, response.status);
}

export async function listOidcIdentities(): Promise<OidcIdentityList> {
  const { data, error, response } = await apiClient.GET('/api/v1/oidc/identities');
  if (!data) throw apiError(error, response.status);
  return data;
}

export async function beginOidcLink(): Promise<string> {
  const { data, error, response } = await apiClient.POST('/api/v1/oidc/link');
  if (!data) throw apiError(error, response.status);
  return data.authorization_url;
}

export async function unlinkOidcIdentity(identityId: string): Promise<void> {
  const { error, response } = await apiClient.DELETE('/api/v1/oidc/identities/{identity_id}', {
    params: { path: { identity_id: identityId } }
  });
  if (!response.ok) throw apiError(error, response.status);
}

export async function runPlayground(input: PlaygroundRequest): Promise<PlaygroundResponse> {
  const { data, error, response } = await apiClient.POST('/api/v1/playground', {
    cache: 'no-store',
    headers: { 'cache-control': 'no-store' },
    body: input
  });
  if (!data) throw apiError(error, response.status);
  return data;
}

export const operationsTesting = { compact };
