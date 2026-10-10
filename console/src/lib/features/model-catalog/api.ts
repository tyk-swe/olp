import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { unwrap } from '$lib/api/http';

export type Catalog = components['schemas']['ModelCatalogResponse'];
export type CatalogModel = components['schemas']['CatalogModel'];
export type Publication = components['schemas']['CatalogPublicationResponse'];
export type Exposure = components['schemas']['RouteCatalogExposureResponse'];
export const catalogKeys = {
  root: ['model-catalog'] as const,
  publication: (id: string) => ['model-catalog', 'publication', id] as const,
  exposure: (id: string) => ['model-catalog', 'exposure', id] as const
};

export async function listCatalog(signal?: AbortSignal): Promise<Catalog> {
  return unwrap(await apiClient.GET('/api/v1/catalog', { signal }));
}

export async function getPublicCatalog(
  projectId: string,
  signal?: AbortSignal
): Promise<Catalog> {
  return unwrap(
    await apiClient.GET('/api/v1/catalog/public/{project_id}', {
      params: { path: { project_id: projectId } },
      signal
    })
  );
}

export async function listExposureRoutes(signal?: AbortSignal) {
  return unwrap(await apiClient.GET('/api/v1/routes', { signal })).items;
}

export async function getPublication(
  id: string,
  signal?: AbortSignal
): Promise<Publication> {
  return unwrap(
    await apiClient.GET('/api/v1/projects/{project_id}/catalog', {
      params: { path: { project_id: id } },
      signal
    })
  );
}

export async function putPublication(
  id: string,
  value: Publication
): Promise<Publication> {
  return unwrap(
    await apiClient.PUT('/api/v1/projects/{project_id}/catalog', {
      params: { path: { project_id: id }, header: { 'If-Match': value.etag } },
      body: { enabled: value.enabled, prices_public: value.prices_public }
    })
  );
}

export async function getExposure(
  id: string,
  signal?: AbortSignal
): Promise<Exposure> {
  return unwrap(
    await apiClient.GET('/api/v1/routes/{route_id}/catalog', {
      params: { path: { route_id: id } },
      signal
    })
  );
}

export async function putExposure(
  id: string,
  value: Exposure
): Promise<Exposure> {
  return unwrap(
    await apiClient.PUT('/api/v1/routes/{route_id}/catalog', {
      params: { path: { route_id: id }, header: { 'If-Match': value.etag } },
      body: { expose_upstream_models: value.expose_upstream_models }
    })
  );
}
