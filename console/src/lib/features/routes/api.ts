import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { ensureSuccess, pageResult, result } from '$lib/api/http';
import { ROUTE_PAGE_SIZE, ROUTE_REVISION_PAGE_SIZE } from '$lib/api/pageSizes';
import { collectCursorPages } from '$lib/api/pagination';
import { type CursorPage } from '$lib/api/http';

type Schemas = components['schemas'];

export type RouteDraft = Schemas['RouteDraftDetailResponse'];
export type RouteDraftValidation = Schemas['RouteDraftResponse'];
export type CreateRouteDraftInput = Schemas['CreateRouteDraftRequest'];
export type ReplaceRouteDraftInput = Schemas['ReplaceRouteDraftRequest'];
export type RouteSimulation = Schemas['RouteSimulationResponse'];
export type RouteSimulationInput = Schemas['SimulateRouteRequest'];
export type RouteRevision = Schemas['RouteRevisionResponse'];
export type RouteRevisionDiff = Schemas['RouteRevisionDiffResponse'];
export type RouteActivation = Schemas['RouteActivationResponse'];
export type ActiveRoute = Schemas['RouteDetailResponse'];

export async function listRouteDraftPage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<RouteDraft>> {
  const response = await apiClient.GET('/api/v3/route-drafts', {
    params: { query: { limit: ROUTE_PAGE_SIZE, cursor } },
    signal
  });
  return pageResult(result(response.data, response.error, response.response));
}

export async function listRoutes(signal?: AbortSignal): Promise<ActiveRoute[]> {
  return collectCursorPages((cursor) => listRoutePage(cursor, signal));
}

export async function listRoutePage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ActiveRoute>> {
  const response = await apiClient.GET('/api/v3/routes', {
    params: { query: { limit: ROUTE_PAGE_SIZE, cursor } },
    signal
  });
  return pageResult(result(response.data, response.error, response.response));
}

export async function getRouteDraft(
  id: string,
  signal?: AbortSignal
): Promise<RouteDraft> {
  const response = await apiClient.GET('/api/v3/route-drafts/{draft_id}', {
    params: { path: { draft_id: id } },
    signal
  });
  return result(response.data, response.error, response.response);
}

export async function createRouteDraft(
  input: CreateRouteDraftInput
): Promise<RouteDraftValidation> {
  const response = await apiClient.POST('/api/v3/route-drafts', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    body: input
  });
  return result(response.data, response.error, response.response);
}

export async function replaceRouteDraft(
  id: string,
  etag: string,
  input: ReplaceRouteDraftInput
): Promise<RouteDraft> {
  const response = await apiClient.PUT('/api/v3/route-drafts/{draft_id}', {
    params: { path: { draft_id: id }, header: { 'If-Match': etag } },
    body: input
  });
  return result(response.data, response.error, response.response);
}

export async function deleteRouteDraft(
  id: string,
  etag: string
): Promise<void> {
  const response = await apiClient.DELETE('/api/v3/route-drafts/{draft_id}', {
    params: { path: { draft_id: id }, header: { 'If-Match': etag } }
  });
  ensureSuccess(response.error, response.response);
}

export async function simulateRoute(
  id: string,
  input: RouteSimulationInput
): Promise<RouteSimulation> {
  const response = await apiClient.POST(
    '/api/v3/route-drafts/{draft_id}/simulate',
    {
      params: { path: { draft_id: id } },
      body: input
    }
  );
  return result(response.data, response.error, response.response);
}

export type RoutingDecision = Schemas['RoutingDecision'];
export type RoutingPreferences = Schemas['RoutingPreferences'];

export type RoutingSimulationInput = Pick<
  Schemas['PlaygroundRequest'],
  'temperature' | 'max_output_tokens' | 'tools' | 'response_format'
> & {
  route: string;
  surface: Schemas['Surface'];
  mode: Schemas['TransportMode'];
  preferences?: RoutingPreferences;
  apiKeyId?: string | null;
  seed?: string;
};

/**
 * Explains routing against the published runtime without spending a provider
 * call, which is what separates this from the playground. The endpoint accepts
 * OLP's canonical operation envelope rather than a console request shape, and
 * the generated contract types that field as an open object, so the envelope is
 * assembled here and nowhere else, and `tests/system/configuration_http_postgres/routes.rs`
 * pins the same payload from the backend side so a protocol change fails there
 * rather than silently at runtime. The envelope is a generation request because
 * that is what the playground composes; a route that serves only another
 * operation will report no eligible attempt.
 */
export async function simulateRouting(
  input: RoutingSimulationInput,
  signal?: AbortSignal
): Promise<RoutingDecision[]> {
  const operation = {
    operation: 'generation',
    request: {
      route: input.route,
      // Provider validation requires a real message shape even for a dry run.
      messages: [{ role: 'user', content: [{ type: 'text', text: 'Hello' }] }],
      parameters: {
        stream: false,
        temperature: input.temperature,
        max_output_tokens: input.max_output_tokens
      },
      tools: input.tools ?? [],
      response_format: input.response_format
    }
  } as unknown as Schemas['SimulationRequest']['operation'];
  const response = await apiClient.POST('/api/v3/routing/simulate', {
    body: {
      operation,
      surface: input.surface,
      mode: input.mode,
      preferences: input.preferences,
      api_key_id: input.apiKeyId ?? null,
      seed: input.seed ?? ''
    },
    signal
  });
  return result(response.data, response.error, response.response);
}

export async function validateRoute(
  draft: RouteDraft
): Promise<RouteDraftValidation> {
  const response = await apiClient.POST(
    '/api/v3/route-drafts/{draft_id}/validate',
    {
      params: {
        path: { draft_id: draft.id },
        header: { 'If-Match': draft.etag }
      }
    }
  );
  return result(response.data, response.error, response.response);
}

export async function activateRoute(
  draft: Pick<RouteDraft, 'id' | 'etag'>
): Promise<RouteActivation> {
  const response = await apiClient.POST(
    '/api/v3/route-drafts/{draft_id}/activate',
    {
      params: {
        path: { draft_id: draft.id },
        header: {
          'If-Match': draft.etag,
          'Idempotency-Key': crypto.randomUUID()
        }
      }
    }
  );
  return result(response.data, response.error, response.response);
}

export async function listRouteRevisions(
  routeId: string,
  signal?: AbortSignal
): Promise<RouteRevision[]> {
  return collectCursorPages((cursor) =>
    listRouteRevisionPage(routeId, cursor, signal)
  );
}

async function listRouteRevisionPage(
  routeId: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<RouteRevision>> {
  const response = await apiClient.GET('/api/v3/routes/{route_id}/revisions', {
    params: {
      path: { route_id: routeId },
      query: { cursor, limit: ROUTE_REVISION_PAGE_SIZE }
    },
    signal
  });
  return pageResult(result(response.data, response.error, response.response));
}

export async function diffRouteRevisions(
  routeId: string,
  from: string,
  to: string,
  signal?: AbortSignal
): Promise<RouteRevisionDiff> {
  const response = await apiClient.GET(
    '/api/v3/routes/{route_id}/revisions/diff',
    {
      params: { path: { route_id: routeId }, query: { from, to } },
      signal
    }
  );
  return result(response.data, response.error, response.response);
}

export async function restoreRouteRevision(
  routeId: string,
  revisionId: string
): Promise<RouteDraft> {
  const response = await apiClient.POST(
    '/api/v3/routes/{route_id}/revisions/{revision_id}/restore-as-draft',
    {
      params: {
        path: { route_id: routeId, revision_id: revisionId },
        header: { 'Idempotency-Key': crypto.randomUUID() }
      }
    }
  );
  return result(response.data, response.error, response.response);
}
