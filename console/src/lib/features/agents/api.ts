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
export type ToolCall = components['schemas']['MCPToolCallResponse'];
export type DelegationConsentRequest =
  components['schemas']['DelegationConsentRequest'];
export type DelegationConsent = components['schemas']['DelegationConsent'];

export async function getDelegationOptions(
  keyID: string,
  signal?: AbortSignal
) {
  return unwrap(
    await apiClient.GET('/api/v1/delegation-options', {
      params: { query: { api_key_id: keyID } },
      signal
    })
  );
}

export async function createDelegationConsent(
  input: DelegationConsentRequest
): Promise<DelegationConsent> {
  return unwrap(
    await apiClient.POST('/api/v1/delegation-consents', { body: input })
  );
}

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
export async function saveServer(
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
export const listRuns = async (project?: string, cursor?: string) =>
  unwrapPage(
    await apiClient.GET('/api/v1/agent-runs', {
      params: { query: { project_id: project || undefined, cursor, limit: 50 } }
    })
  );

export const getRun = async (runID: string) =>
  unwrap(
    await apiClient.GET('/api/v1/agent-runs/{run_id}', {
      params: { path: { run_id: runID } }
    })
  );

export const listRunSpans = async (runID: string, cursor?: string) =>
  unwrapPage(
    await apiClient.GET('/api/v1/agent-runs/{run_id}/spans', {
      params: { path: { run_id: runID }, query: { cursor, limit: 50 } }
    })
  );

export type InvestigationWrite = components['schemas']['InvestigationWrite'];
export type InvestigationEvidence =
  components['schemas']['InvestigationEvidence'];

export const listInvestigations = async (project?: string, cursor?: string) =>
  unwrapPage(
    await apiClient.GET('/api/v1/investigations', {
      params: { query: { project_id: project || undefined, cursor, limit: 50 } }
    })
  );

export const getInvestigation = async (id: string) =>
  unwrap(
    await apiClient.GET('/api/v1/investigations/{investigation_id}', {
      params: { path: { investigation_id: id } }
    })
  );

export const createInvestigation = async (
  input: InvestigationWrite,
  requestId: string
) =>
  unwrap(
    await apiClient.POST('/api/v1/investigations', {
      params: { header: { 'Idempotency-Key': requestId } },
      body: input
    })
  );

export const listExposures = async (key?: string, cursor?: string) =>
  unwrapPage(
    await apiClient.GET('/api/v1/execution-debits', {
      params: { query: { api_key_id: key || undefined, cursor, limit: 50 } }
    })
  );

export const listToolCalls = async (
  project?: string,
  key?: string,
  cursor?: string
) =>
  unwrapPage(
    await apiClient.GET('/api/v1/mcp-tool-calls', {
      params: {
        query: {
          project_id: project || undefined,
          api_key_id: key || undefined,
          cursor,
          limit: 50
        }
      }
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

export type MCPGrantStanding = components['schemas']['MCPUserGrantStanding'];
export const listMCPGrants = () =>
  collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/mcp-user-grants', {
        params: { query: { cursor, limit: 200 } }
      })
    )
  );
export const startMCPGrant = async (serverID: string, keyID: string) =>
  unwrap(
    await apiClient.POST('/api/v1/mcp-servers/{server_id}/grant-enrollments', {
      params: { path: { server_id: serverID } },
      body: { api_key_id: keyID }
    })
  );
export const startAgentGrant = async (agentID: string, keyID: string) =>
  unwrap(
    await apiClient.POST('/api/v1/agents/{agent_id}/grant-enrollments', {
      params: { path: { agent_id: agentID } },
      body: { api_key_id: keyID }
    })
  );
export const claimMCPGrant = async (
  enrollmentID: string,
  handoffToken: string
) =>
  unwrap(
    await apiClient.POST('/api/v1/mcp-grant-enrollments/claim', {
      body: { enrollment_id: enrollmentID, handoff_token: handoffToken }
    })
  );
export const revokeMCPGrant = async (grantID: string) =>
  unwrap(
    await apiClient.DELETE('/api/v1/mcp-user-grants/{grant_id}', {
      params: { path: { grant_id: grantID } }
    })
  );

export type MCPGrantAuthorization =
  components['schemas']['MCPGrantAuthorization'];
export async function authorizeMCPGrant(
  enrollmentID: string,
  flow: 'authorization_code' | 'device_code'
) {
  return unwrap(
    await apiClient.POST(
      '/api/v1/mcp-grant-enrollments/{enrollment_id}/authorize',
      {
        params: { path: { enrollment_id: enrollmentID } },
        body: { flow }
      }
    )
  );
}
export async function completeMCPGrant(
  enrollmentID: string,
  code: string,
  state: string
) {
  return unwrap(
    await apiClient.POST(
      '/api/v1/mcp-grant-enrollments/{enrollment_id}/complete',
      {
        params: { path: { enrollment_id: enrollmentID } },
        body: { code, state }
      }
    )
  );
}
export async function pollMCPGrant(enrollmentID: string) {
  return unwrap(
    await apiClient.POST('/api/v1/mcp-grant-enrollments/{enrollment_id}/poll', {
      params: { path: { enrollment_id: enrollmentID } }
    })
  );
}
