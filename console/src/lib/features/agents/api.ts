import { apiClient } from '$lib/api/client';
import type { components } from '$lib/api/schema';
import { ensureOk, unwrap, unwrapPage } from '$lib/api/http';
import { collectCursorPages } from '$lib/api/pagination';

export type Kind = 'toolsets' | 'agents' | 'prompts';
export type Publication = components['schemas']['AgentPublication'];
export type Draft = components['schemas']['AgentPublicationWrite'];
export type ToolPin = components['schemas']['AgentToolPin'];
export type MCPServer = components['schemas']['MCPServerResponse'];
export type MCPServerUpdate = components['schemas']['UpdateMCPServerRequest'];
export type Revision = components['schemas']['AgentPublicationRevision'];
export type Exposure = components['schemas']['ExecutionDebit'];

const collections = {
  toolsets: '/api/v1/toolsets',
  agents: '/api/v1/agents',
  prompts: '/api/v1/prompts'
} as const;
const details = {
  toolsets: '/api/v1/toolsets/{publication_id}',
  agents: '/api/v1/agents/{publication_id}',
  prompts: '/api/v1/prompts/{publication_id}'
} as const;
const publications = {
  toolsets: '/api/v1/toolsets/{publication_id}/publish',
  agents: '/api/v1/agents/{publication_id}/publish',
  prompts: '/api/v1/prompts/{publication_id}/publish'
} as const;
const revisions = {
  toolsets: '/api/v1/toolsets/{publication_id}/revisions/{revision_id}',
  agents: '/api/v1/agents/{publication_id}/revisions/{revision_id}',
  prompts: '/api/v1/prompts/{publication_id}/revisions/{revision_id}'
} as const;

export const listPublications = (kind: Kind) =>
  collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET(collections[kind], {
        params: { query: { cursor, limit: 200 } }
      })
    )
  );
export const getPublication = async (kind: Kind, id: string) =>
  unwrap(
    await apiClient.GET(details[kind], {
      params: { path: { publication_id: id } }
    })
  );
export const getRevision = async (kind: Kind, id: string, revision: string) =>
  unwrap(
    await apiClient.GET(revisions[kind], {
      params: { path: { publication_id: id, revision_id: revision } }
    })
  );
export const listProjects = () =>
  collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/projects', {
        params: { query: { cursor, limit: 200 } }
      })
    )
  );
export const listServers = () =>
  collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/mcp-servers', {
        params: { query: { cursor, limit: 200 } }
      })
    )
  );
export const getServer = async (id: string) =>
  unwrap(
    await apiClient.GET('/api/v1/mcp-servers/{server_id}', {
      params: { path: { server_id: id } }
    })
  );
export async function saveServerPricing(
  current: MCPServer,
  input: MCPServerUpdate,
  requestId: string
) {
  return unwrap(
    await apiClient.PUT('/api/v1/mcp-servers/{server_id}', {
      params: {
        path: { server_id: current.id },
        header: {
          'If-Match': `"${current.etag}"`,
          'Idempotency-Key': requestId
        }
      },
      body: input
    })
  );
}
export const listExposures = async (key?: string, cursor?: string) =>
  unwrapPage(
    await apiClient.GET('/api/v1/execution-debits', {
      params: { query: { api_key_id: key || undefined, cursor, limit: 50 } }
    })
  );

export async function savePublication(
  kind: Kind,
  current: Publication | null,
  input: Draft,
  requestId: string
) {
  return current
    ? unwrap(
        await apiClient.PUT(details[kind], {
          params: {
            path: { publication_id: current.id },
            header: {
              'If-Match': `"${current.etag}"`,
              'Idempotency-Key': requestId
            }
          },
          body: input
        })
      )
    : unwrap(
        await apiClient.POST(collections[kind], {
          params: { header: { 'Idempotency-Key': requestId } },
          body: input
        })
      );
}
export async function publish(
  kind: Kind,
  current: Publication,
  requestId: string
) {
  return unwrap(
    await apiClient.POST(publications[kind], {
      params: {
        path: { publication_id: current.id },
        header: {
          'If-Match': `"${current.etag}"`,
          'Idempotency-Key': requestId
        }
      }
    })
  );
}
export async function retire(
  kind: Kind,
  current: Publication,
  requestId: string
) {
  ensureOk(
    await apiClient.DELETE(details[kind], {
      params: {
        path: { publication_id: current.id },
        header: {
          'If-Match': `"${current.etag}"`,
          'Idempotency-Key': requestId
        }
      }
    })
  );
}
export async function setLabel(
  current: Publication,
  label: 'production' | 'staging',
  revision: string,
  requestId: string
) {
  return unwrap(
    await apiClient.PUT('/api/v1/prompts/{publication_id}/labels/{label}', {
      params: {
        path: { publication_id: current.id, label },
        header: {
          'If-Match': `"${current.etag}"`,
          'Idempotency-Key': requestId
        }
      },
      body: { revision_id: revision }
    })
  );
}
