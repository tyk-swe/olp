import type { components } from '../schema';
import { apiClient } from '../client';
import { throwApiProblem } from '../http';
import {
  collectCursorPages,
  getAbortSignal,
  requireResponseData,
  type CursorPage,
  type ReadSignal
} from './shared';

type Schemas = components['schemas'];

export type RouteDraft = Schemas['RouteDraftDetailResponse'];
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
  const response = await apiClient.GET('/api/v1/route-drafts', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  const page = requireResponseData(response.data, response.error, response.response);
  return { items: page.items, nextCursor: page.next_cursor ?? null };
}

export async function listRoutes(signal?: ReadSignal): Promise<ActiveRoute[]> {
  return collectCursorPages((cursor) => listRoutePage(cursor, getAbortSignal(signal)));
}

export async function listRoutePage(
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<ActiveRoute>> {
  const response = await apiClient.GET('/api/v1/routes', {
    params: { query: { limit: 50, cursor } },
    signal
  });
  const page = requireResponseData(response.data, response.error, response.response);
  return { items: page.items, nextCursor: page.next_cursor ?? null };
}

export async function getRouteDraft(id: string, signal?: AbortSignal): Promise<RouteDraft> {
  const response = await apiClient.GET('/api/v1/route-drafts/{draft_id}', {
    params: { path: { draft_id: id } },
    signal
  });
  return requireResponseData(response.data, response.error, response.response);
}

export async function createRouteDraft(input: CreateRouteDraftInput): Promise<string> {
  const response = await apiClient.POST('/api/v1/route-drafts', {
    params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
    body: input
  });
  return requireResponseData(response.data, response.error, response.response).id;
}

export async function replaceRouteDraft(id: string, etag: string, input: ReplaceRouteDraftInput): Promise<RouteDraft> {
  const response = await apiClient.PUT('/api/v1/route-drafts/{draft_id}', {
    params: { path: { draft_id: id }, header: { 'If-Match': etag } },
    body: input
  });
  return requireResponseData(response.data, response.error, response.response);
}

export async function deleteRouteDraft(id: string, etag: string): Promise<void> {
  const response = await apiClient.DELETE('/api/v1/route-drafts/{draft_id}', {
    params: { path: { draft_id: id }, header: { 'If-Match': etag } }
  });
  if (!response.response.ok) throwApiProblem(response.error, response.response);
}

export async function simulateRoute(id: string, input: RouteSimulationInput): Promise<RouteSimulation> {
  const response = await apiClient.POST('/api/v1/route-drafts/{draft_id}/simulate', {
    params: { path: { draft_id: id } },
    body: input
  });
  return requireResponseData(response.data, response.error, response.response);
}

export async function validateRoute(draft: RouteDraft): Promise<void> {
  const response = await apiClient.POST('/api/v1/route-drafts/{draft_id}/validate', {
    params: {
      path: { draft_id: draft.id },
      header: { 'If-Match': draft.etag }
    }
  });
  requireResponseData(response.data, response.error, response.response);
}

export async function activateRoute(draft: RouteDraft): Promise<RouteActivation> {
  const response = await apiClient.POST('/api/v1/route-drafts/{draft_id}/activate', {
    params: {
      path: { draft_id: draft.id },
      header: { 'If-Match': draft.etag, 'Idempotency-Key': crypto.randomUUID() }
    }
  });
  return requireResponseData(response.data, response.error, response.response);
}

export async function listRouteRevisions(
  routeId: string,
  signal?: AbortSignal
): Promise<RouteRevision[]> {
  return collectCursorPages((cursor) => listRouteRevisionPage(routeId, cursor, signal));
}

async function listRouteRevisionPage(
  routeId: string,
  cursor?: string,
  signal?: AbortSignal
): Promise<CursorPage<RouteRevision>> {
  const response = await apiClient.GET('/api/v1/routes/{route_id}/revisions', {
    params: { path: { route_id: routeId }, query: { cursor, limit: 100 } },
    signal
  });
  const page = requireResponseData(response.data, response.error, response.response);
  return { items: page.items, nextCursor: page.next_cursor ?? null };
}

export async function diffRouteRevisions(
  routeId: string,
  from: string,
  to: string,
  signal?: AbortSignal
): Promise<RouteRevisionDiff> {
  const response = await apiClient.GET('/api/v1/routes/{route_id}/revisions/diff', {
    params: { path: { route_id: routeId }, query: { from, to } },
    signal
  });
  return requireResponseData(response.data, response.error, response.response);
}

export async function restoreRouteRevision(routeId: string, revisionId: string): Promise<RouteDraft> {
  const response = await apiClient.POST('/api/v1/routes/{route_id}/revisions/{revision_id}/restore-as-draft', {
    params: {
      path: { route_id: routeId, revision_id: revisionId },
      header: { 'Idempotency-Key': crypto.randomUUID() }
    }
  });
  return requireResponseData(response.data, response.error, response.response);
}
