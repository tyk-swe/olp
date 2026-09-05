import type { components } from './schema';
import { apiClient } from './client';
import { pageResult, result } from './http';
import {
  GATEWAY_EPOCH_PAGE_SIZE,
  PROVIDER_HEALTH_PAGE_SIZE
} from './pageSizes';
import { collectCursorPages } from './pagination';
import { type CursorPage } from '$lib/api/http';

export type RequestMetadataGatewayEpoch =
  components['schemas']['RequestMetadataGatewayEpochResponse'];
export type RequestMetadataEpochAcknowledgement =
  components['schemas']['RequestMetadataEpochAcknowledgementResponse'];
export type ProviderHealth = components['schemas']['ProviderHealthItem'];
export type Readiness = components['schemas']['HealthResponse'];

export type RequestMetadataGatewayEpochState =
  'open' | 'gracefully_closed' | 'unresolved' | 'acknowledged';

export async function listRequestMetadataGatewayEpochs(
  state: RequestMetadataGatewayEpochState,
  cursor?: string
): Promise<CursorPage<RequestMetadataGatewayEpoch>> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/request-metadata/gateway-epochs',
    { params: { query: { state, cursor, limit: GATEWAY_EPOCH_PAGE_SIZE } } }
  );
  return pageResult(result(data, error, response));
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

export async function getReadiness(): Promise<Readiness> {
  const { data, error, response } = await apiClient.GET('/api/v1/health/ready');
  return result(data, error, response);
}

export async function listProviderHealth(windowMinutes = 15): Promise<{
  window_minutes: number;
  items: ProviderHealth[];
}> {
  let responseWindow = windowMinutes;
  const items = await collectCursorPages(async (cursor) => {
    const response = await apiClient.GET('/api/v1/provider-health', {
      params: {
        query: {
          window_minutes: windowMinutes,
          cursor,
          limit: PROVIDER_HEALTH_PAGE_SIZE
        }
      }
    });
    const page = result(response.data, response.error, response.response);
    responseWindow = page.window_minutes;
    return pageResult(page);
  });
  return { window_minutes: responseWindow, items };
}
