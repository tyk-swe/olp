import type { components } from '../schema';
import { apiClient } from '../client';
import { pageResult, result, type CursorPage } from '../http';
import { PROVIDER_REVISION_PAGE_SIZE } from '../pageSizes';
import type { Provider } from './providers';
import type { ProviderModel } from './providerModels';

type Schemas = components['schemas'];

export type ProviderRevision = Schemas['ProviderRevisionSummaryResponse'];

export type ProviderRevisionDiff = Schemas['ProviderRevisionDiffResponse'];

export type ProviderRevisionDetail = Schemas['ProviderRevisionResponse'];

export type ProviderRevisionRestore =
  Schemas['ProviderRevisionRestoreResponse'];

export async function listProviderRevisionPage(
  providerId: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ProviderRevision>> {
  const response = await apiClient.GET(
    '/api/v1/providers/{provider_id}/revisions',
    {
      params: {
        path: { provider_id: providerId },
        query: { cursor, limit: PROVIDER_REVISION_PAGE_SIZE }
      },
      signal
    }
  );
  return pageResult(result(response.data, response.error, response.response));
}

export async function getProviderRevision(
  providerId: string,
  revisionId: string,
  signal?: AbortSignal
): Promise<ProviderRevisionDetail> {
  const response = await apiClient.GET(
    '/api/v1/providers/{provider_id}/revisions/{revision_id}',
    {
      params: {
        path: { provider_id: providerId, revision_id: revisionId }
      },
      signal
    }
  );
  return result(response.data, response.error, response.response);
}

export async function listProviderRevisionModelPage(
  providerId: string,
  revisionId: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ProviderModel>> {
  const response = await apiClient.GET(
    '/api/v1/providers/{provider_id}/revisions/{revision_id}/models',
    {
      params: {
        path: { provider_id: providerId, revision_id: revisionId },
        query: { cursor, limit: PROVIDER_REVISION_PAGE_SIZE }
      },
      signal
    }
  );
  return pageResult(result(response.data, response.error, response.response));
}

export async function diffProviderRevisions(
  providerId: string,
  from: string,
  to: string,
  signal?: AbortSignal
): Promise<ProviderRevisionDiff> {
  const response = await apiClient.GET(
    '/api/v1/providers/{provider_id}/revisions/diff',
    {
      params: { path: { provider_id: providerId }, query: { from, to } },
      signal
    }
  );
  return result(response.data, response.error, response.response);
}

export async function restoreProviderRevision(
  provider: Provider,
  revisionId: string
): Promise<ProviderRevisionRestore> {
  const response = await apiClient.POST(
    '/api/v1/providers/{provider_id}/revisions/{revision_id}/restore-as-draft',
    {
      params: {
        path: { provider_id: provider.id, revision_id: revisionId },
        header: {
          'If-Match': provider.etag,
          'Idempotency-Key': crypto.randomUUID()
        }
      }
    }
  );
  return result(
    response.data,
    response.error,
    response.response
  ) as ProviderRevisionRestore;
}
