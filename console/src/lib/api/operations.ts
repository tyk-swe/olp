import type { components } from './schema';
import { apiClient } from './client';
import { ensureSuccess, result } from './http';
import { collectCursorPages } from './pagination';

export type RequestSummary = components['schemas']['RequestSummary'];
export type RequestDetail = components['schemas']['RequestDetailResponse'];
export type AuditEvent = components['schemas']['AuditEventResponse'];
export type RuntimeGeneration = components['schemas']['RuntimeGenerationItem'];
export type MediaJob = components['schemas']['MediaJobItem'];
export type Setting = components['schemas']['SettingResponse'];
export type PricingRevision = components['schemas']['PricingRevisionResponse'];
export type UsagePoint = components['schemas']['UsagePointResponse'];
export type UsageBreakdownItem = components['schemas']['UsageBreakdownItem'];
export type RequestMetadataGatewayEpoch =
  components['schemas']['RequestMetadataGatewayEpochResponse'];
export type RequestMetadataEpochAcknowledgement =
  components['schemas']['RequestMetadataEpochAcknowledgementResponse'];
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

export type RequestMetadataGatewayEpochState =
  | 'open'
  | 'gracefully_closed'
  | 'unresolved'
  | 'acknowledged';

type UsageSummary = components['schemas']['UsageSummaryResponse'];
type UsageCompleteness = components['schemas']['UsageCompletenessResponse'];
type UsageSeriesResult = components['schemas']['UsageTimeSeriesResponse'];
type UsageBreakdownResult = components['schemas']['UsageBreakdownResponse'];

export type ProviderHealth = components['schemas']['ProviderHealthItem'];

export type PlaygroundRequest = Omit<components['schemas']['PlaygroundRequest'], 'surface'> & {
  surface?: 'openai' | 'anthropic' | 'gemini';
};
export type PlaygroundResponse = components['schemas']['PlaygroundResponse'];

export type PriceDraft = components['schemas']['PriceRequest'];

export type ProfileUpdate = { display_name: string };
export type PasswordChange = { current_password: string; new_password: string };
export type PasswordEnrollment = { new_password: string };
export type RecentAuthenticationPurpose =
  | 'password_enrollment'
  | 'oidc_link'
  | 'oidc_unlink';

function compact<T extends Record<string, unknown>>(value: T): T {
  return Object.fromEntries(
    Object.entries(value).filter(([, item]) => item !== '' && item !== undefined && item !== null)
  ) as T;
}

export async function listRequests(filters: RequestFilters): Promise<CursorPage<RequestSummary>> {
  const { data, error, response } = await apiClient.GET('/api/v1/requests', {
    params: { query: compact(filters) }
  });
  return result(data, error, response);
}

export async function getRequest(requestId: string): Promise<RequestDetail> {
  const { data, error, response } = await apiClient.GET('/api/v1/requests/{request_id}', {
    params: { path: { request_id: requestId } }
  });
  return result(data, error, response);
}

export async function listMediaJobs(filters: MediaJobFilters): Promise<CursorPage<MediaJob>> {
  const { data, error, response } = await apiClient.GET('/api/v1/media-jobs', {
    params: { query: compact(filters) }
  });
  return result(data, error, response);
}

export async function getMediaJob(jobId: string): Promise<MediaJob> {
  const { data, error, response } = await apiClient.GET('/api/v1/media-jobs/{job_id}', {
    params: { path: { job_id: jobId } }
  });
  return result(data, error, response);
}

export async function usageSummary(filters: UsageFilters): Promise<UsageSummary> {
  const { data, error, response } = await apiClient.GET('/api/v1/usage/summary', {
    params: { query: compact(filters) }
  });
  return result(data, error, response);
}

export async function listRequestMetadataGatewayEpochs(
  state: RequestMetadataGatewayEpochState,
  cursor?: string
): Promise<CursorPage<RequestMetadataGatewayEpoch>> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/request-metadata/gateway-epochs',
    { params: { query: { state, cursor, limit: 25 } } }
  );
  return result(data, error, response);
}

export async function acknowledgeRequestMetadataGatewayEpoch(
  processEpoch: string
): Promise<RequestMetadataEpochAcknowledgement> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/request-metadata/gateway-epochs/{process_epoch}/acknowledge',
    { params: { path: { process_epoch: processEpoch } } }
  );
  return result(data, error, response);
}

export async function usageSeries(
  filters: UsageFilters,
  granularity: 'hour' | 'day'
): Promise<UsageSeriesResult> {
  const { data, error, response } = await apiClient.GET('/api/v1/usage/time-series', {
    params: { query: compact({ ...filters, granularity }) }
  });
  return result(data, error, response);
}

export async function usageBreakdown(
  filters: UsageFilters,
  dimension: 'route' | 'provider' | 'model' | 'api_key' | 'operation'
): Promise<UsageBreakdownResult> {
  const { data, error, response } = await apiClient.GET('/api/v1/usage/breakdown', {
    params: { query: compact({ ...filters, dimension, limit: 50 }) }
  });
  return result(data, error, response);
}

export async function usageCompleteness(filters: UsageFilters): Promise<UsageCompleteness> {
  const { data, error, response } = await apiClient.GET('/api/v1/usage/completeness', {
    params: { query: compact(filters) }
  });
  return result(data, error, response);
}

export async function listAudit(cursor?: string): Promise<CursorPage<AuditEvent>> {
  const { data, error, response } = await apiClient.GET('/api/v1/audit', {
    params: { query: compact({ cursor, limit: 50 }) }
  });
  return result(data, error, response);
}

export async function getReadiness(): Promise<Readiness> {
  const { data, error, response } = await apiClient.GET('/api/v1/health/ready');
  return result(data, error, response);
}

export async function listProviderHealth(windowMinutes = 15): Promise<{
  window_minutes: number;
  data: ProviderHealth[];
}> {
  let responseWindow = windowMinutes;
  const data = await collectCursorPages(async (cursor) => {
    const response = await apiClient.GET('/api/v1/provider-health', {
      params: { query: { window_minutes: windowMinutes, cursor, limit: 200 } }
    });
    const page = result(response.data, response.error, response.response);
    responseWindow = page.window_minutes;
    return { items: page.data, nextCursor: page.next_cursor ?? null };
  });
  return { window_minutes: responseWindow, data };
}

export async function listRuntimeGenerations(cursor?: string): Promise<CursorPage<RuntimeGeneration>> {
  const { data, error, response } = await apiClient.GET('/api/v1/runtime-generations', {
    params: { query: compact({ cursor, limit: 25 }) }
  });
  return result(data, error, response);
}

export async function listSettings(): Promise<Setting[]> {
  const { data, error, response } = await apiClient.GET('/api/v1/settings');
  return result(data, error, response).data;
}

export async function updateSetting(setting: Setting, value: string): Promise<Setting> {
  const { data, error, response } = await apiClient.PUT('/api/v1/settings/{key}', {
    params: {
      path: { key: setting.key },
      header: { 'If-Match': setting.etag }
    },
    body: { value }
  });
  return result(data, error, response);
}

export async function listPricing(cursor?: string): Promise<CursorPage<PricingRevision>> {
  const { data, error, response } = await apiClient.GET('/api/v1/pricing/revisions', {
    params: { query: compact({ cursor, limit: 25 }) }
  });
  return result(data, error, response);
}

export async function createPricingRevision(
  effectiveAt: string,
  prices: PriceDraft[]
): Promise<PricingRevision> {
  const { data, error, response } = await apiClient.POST('/api/v1/pricing/revisions', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    body: { effective_at: effectiveAt, prices }
  });
  return result(data, error, response);
}

export async function getProfile(): Promise<UserProfile> {
  const { data, error, response } = await apiClient.GET('/api/v1/profile');
  return result(data, error, response);
}

export async function updateProfile(profile: UserProfile, input: ProfileUpdate): Promise<UserProfile> {
  const { data, error, response } = await apiClient.PATCH('/api/v1/profile', {
    params: { header: { 'If-Match': profile.etag } },
    body: input
  });
  return result(data, error, response);
}

export async function reauthenticateWithPassword(
  currentPassword: string,
  purpose: RecentAuthenticationPurpose,
  resourceId?: string
): Promise<void> {
  const { error, response } = await apiClient.POST('/api/v1/profile/reauthenticate', {
    body: {
      current_password: currentPassword,
      purpose,
      ...(resourceId ? { resource_id: resourceId } : {})
    }
  });
  ensureSuccess(error, response);
}

export async function changePassword(profile: UserProfile, input: PasswordChange): Promise<UserProfile> {
  const { data, error, response } = await apiClient.POST('/api/v1/profile/password', {
    params: { header: { 'If-Match': profile.etag } },
    body: input
  });
  return result(data, error, response);
}

export async function enrollPassword(
  profile: UserProfile,
  input: PasswordEnrollment
): Promise<UserProfile> {
  const { data, error, response } = await apiClient.POST('/api/v1/profile/password/enroll', {
    params: { header: { 'If-Match': profile.etag } },
    body: input
  });
  return result(data, error, response);
}

export async function listSessions(cursor?: string): Promise<CursorPage<Session>> {
  const { data, error, response } = await apiClient.GET('/api/v1/sessions', {
    params: { query: compact({ cursor, limit: 50 }) }
  });
  return result(data, error, response);
}

export async function revokeSession(id: string): Promise<void> {
  const { error, response } = await apiClient.DELETE('/api/v1/sessions/{session_id}', {
    params: { path: { session_id: id } }
  });
  ensureSuccess(error, response);
}

export async function listOidcIdentities(): Promise<OidcIdentityList> {
  const { data, error, response } = await apiClient.GET('/api/v1/oidc/identities');
  return result(data, error, response);
}

export async function beginOidcReauthentication(
  purpose: RecentAuthenticationPurpose,
  resourceId?: string
): Promise<string> {
  const { data, error, response } = await apiClient.POST('/api/v1/oidc/reauthenticate', {
    body: { purpose, ...(resourceId ? { resource_id: resourceId } : {}) }
  });
  return result(data, error, response).authorization_url;
}

export async function beginOidcLink(): Promise<string> {
  const { data, error, response } = await apiClient.POST('/api/v1/oidc/link');
  return result(data, error, response).authorization_url;
}

export async function unlinkOidcIdentity(identityId: string): Promise<void> {
  const { error, response } = await apiClient.DELETE('/api/v1/oidc/identities/{identity_id}', {
    params: { path: { identity_id: identityId } }
  });
  ensureSuccess(error, response);
}

export async function runPlayground(input: PlaygroundRequest): Promise<PlaygroundResponse> {
  const { data, error, response } = await apiClient.POST('/api/v1/playground', {
    cache: 'no-store',
    headers: { 'cache-control': 'no-store' },
    body: input
  });
  return result(data, error, response);
}

export const operationsTesting = { compact };
