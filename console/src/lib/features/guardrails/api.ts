import { apiClient } from '#lib/management/client.js';
import type { components, operations } from '#lib/management/schema.js';
import {
  createHeaders,
  ensureOk,
  mutationHeaders,
  unwrap,
  unwrapPage
} from '#lib/management/http.js';
import { collectCursorPages } from '#lib/management/pagination.js';

export type Definition = components['schemas']['GuardrailResponse'];
export type Policy = components['schemas']['GuardrailPolicy'];
export type Entry = components['schemas']['GuardrailPolicyEntry'];
export type Result = components['schemas']['GuardrailApplyResult'];
export type PreviewSelection =
  operations['preview_guardrails']['parameters']['query'];
export type PreviewEntry = components['schemas']['GuardrailPreviewEntry'];
export const getGuardrailPreview = async (
  query: PreviewSelection,
  signal?: AbortSignal
) =>
  unwrap(
    await apiClient.GET('/api/v1/guardrails/preview', {
      params: { query },
      signal
    })
  );
export const listDefinitions = (signal?: AbortSignal): Promise<Definition[]> =>
  collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/guardrails', {
        params: { query: { cursor } },
        signal
      })
    )
  );
export const listPolicies = async () => ({
  items: await collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/guardrail-policies', {
        params: { query: { cursor } }
      })
    )
  )
});
export const listAttachments = async () =>
  unwrap(await apiClient.GET('/api/v1/guardrail-attachments'));
export async function saveDefinition(
  current: Definition | null,
  project: string,
  type: Definition['type'],
  body: components['schemas']['UpdateGuardrailRequest'],
  requestId: string
) {
  return current
    ? unwrap(
        await apiClient.PUT('/api/v1/guardrails/{guardrail_id}', {
          params: {
            path: { guardrail_id: current.id },
            header: mutationHeaders(current.etag, requestId)
          },
          body
        })
      )
    : unwrap(
        await apiClient.POST('/api/v1/guardrails', {
          params: { header: createHeaders(requestId) },
          body: { ...body, project_id: project, type }
        })
      );
}
export async function savePolicy(
  current: Policy | null,
  project: string,
  body: components['schemas']['GuardrailPolicyInput'],
  requestId: string
) {
  return current
    ? unwrap(
        await apiClient.PUT('/api/v1/guardrail-policies/{policy_id}', {
          params: {
            path: { policy_id: current.policy_id },
            header: mutationHeaders(current.etag, requestId)
          },
          body
        })
      )
    : unwrap(
        await apiClient.POST('/api/v1/guardrail-policies', {
          params: { header: createHeaders(requestId) },
          body: { ...body, project_id: project }
        })
      );
}
export async function activateDefinition(
  current: Definition,
  requestId: string
) {
  ensureOk(
    await apiClient.POST('/api/v1/guardrails/{guardrail_id}/activate', {
      params: {
        path: { guardrail_id: current.id },
        header: mutationHeaders(current.etag, requestId)
      },
      body: { revision_id: current.revision_id }
    })
  );
}
export async function activatePolicy(current: Policy, requestId: string) {
  ensureOk(
    await apiClient.POST('/api/v1/guardrail-policies/{policy_id}/activate', {
      params: {
        path: { policy_id: current.policy_id },
        header: mutationHeaders(current.etag, requestId)
      },
      body: { revision_id: current.revision_id }
    })
  );
}
export async function attachPolicy(
  body: components['schemas']['GuardrailAttachment'],
  requestId: string
) {
  ensureOk(
    await apiClient.POST('/api/v1/guardrail-attachments', {
      params: { header: createHeaders(requestId) },
      body
    })
  );
}
export async function evaluatePolicy(
  body: components['schemas']['GuardrailApplyInput']
) {
  return unwrap(await apiClient.POST('/api/v1/guardrails/evaluate', { body }));
}

export type Credential = components['schemas']['GuardrailCredential'];
export type Attachment = components['schemas']['GuardrailAttachment'];
export const listCredentials = () =>
  collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/guardrail-credentials', {
        params: { query: { cursor } }
      })
    )
  );
export const createCredential = async (
  body: components['schemas']['GuardrailCredentialInput'],
  requestId: string
) =>
  unwrap(
    await apiClient.POST('/api/v1/guardrail-credentials', {
      body,
      params: { header: createHeaders(requestId) }
    })
  );
export const deleteCredential = async (
  credential: Credential,
  requestId: string
) =>
  ensureOk(
    await apiClient.DELETE('/api/v1/guardrail-credentials/{credential_id}', {
      params: {
        path: { credential_id: credential.id },
        header: mutationHeaders(credential.id, requestId)
      }
    })
  );
export const retireDefinition = async (value: Definition, requestId: string) =>
  ensureOk(
    await apiClient.DELETE('/api/v1/guardrails/{guardrail_id}', {
      params: {
        path: { guardrail_id: value.id },
        header: mutationHeaders(value.etag, requestId)
      }
    })
  );
export const retirePolicy = async (value: Policy, requestId: string) =>
  ensureOk(
    await apiClient.DELETE('/api/v1/guardrail-policies/{policy_id}', {
      params: {
        path: { policy_id: value.policy_id },
        header: mutationHeaders(value.etag, requestId)
      }
    })
  );
export const detachPolicy = async (
  value: Attachment & { id: string; etag: string },
  requestId: string
) =>
  ensureOk(
    await apiClient.DELETE('/api/v1/guardrail-attachments/{attachment_id}', {
      params: {
        path: { attachment_id: value.id },
        header: mutationHeaders(value.etag, requestId)
      }
    })
  );
export const replaceAttachment = async (
  value: Attachment & { id: string; etag: string },
  revision: string,
  requestId: string
) =>
  unwrap(
    await apiClient.PUT('/api/v1/guardrail-attachments/{attachment_id}', {
      body: { policy_revision_id: revision },
      params: {
        path: { attachment_id: value.id },
        header: mutationHeaders(value.etag, requestId)
      }
    })
  );
export const getPolicyRevision = async (policy: string, revision: string) =>
  unwrap(
    await apiClient.GET(
      '/api/v1/guardrail-policies/{policy_id}/revisions/{revision_id}',
      { params: { path: { policy_id: policy, revision_id: revision } } }
    )
  );
