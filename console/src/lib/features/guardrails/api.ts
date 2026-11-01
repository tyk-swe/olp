import { apiClient } from '$lib/api/client';
import type { components } from '$lib/api/schema';
import { ensureOk, unwrap, unwrapPage } from '$lib/api/http';
import { collectCursorPages } from '$lib/api/pagination';

export type Definition = components['schemas']['GuardrailResponse'];
export type Policy = components['schemas']['GuardrailPolicy'];
export type Entry = components['schemas']['GuardrailPolicyEntry'];
export type Result = components['schemas']['GuardrailApplyResult'];
export const listDefinitions = async () => ({
  items: await collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/guardrails', {
        params: { query: { cursor } }
      })
    )
  )
});
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
export const listProjects = async () => ({
  items: await collectCursorPages(async (cursor) =>
    unwrapPage(
      await apiClient.GET('/api/v1/projects', {
        params: { query: { cursor } }
      })
    )
  )
});
export async function saveDefinition(
  current: Definition | null,
  project: string,
  type: Definition['type'],
  body: components['schemas']['UpdateGuardrailRequest']
) {
  return current
    ? unwrap(
        await apiClient.PUT('/api/v1/guardrails/{guardrail_id}', {
          params: {
            path: { guardrail_id: current.id },
            header: {
              'If-Match': `"${current.etag}"`,
              'Idempotency-Key': crypto.randomUUID()
            }
          },
          body
        })
      )
    : unwrap(
        await apiClient.POST('/api/v1/guardrails', {
          params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
          body: { ...body, project_id: project, type }
        })
      );
}
export async function savePolicy(
  current: Policy | null,
  project: string,
  body: components['schemas']['GuardrailPolicyInput']
) {
  return current
    ? unwrap(
        await apiClient.PUT('/api/v1/guardrail-policies/{policy_id}', {
          params: {
            path: { policy_id: current.policy_id },
            header: {
              'If-Match': `"${current.etag}"`,
              'Idempotency-Key': crypto.randomUUID()
            }
          },
          body
        })
      )
    : unwrap(
        await apiClient.POST('/api/v1/guardrail-policies', {
          params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
          body: { ...body, project_id: project }
        })
      );
}
export async function activateDefinition(current: Definition) {
  ensureOk(
    await apiClient.POST('/api/v1/guardrails/{guardrail_id}/activate', {
      params: {
        path: { guardrail_id: current.id },
        header: {
          'If-Match': `"${current.etag}"`,
          'Idempotency-Key': crypto.randomUUID()
        }
      },
      body: { revision_id: current.revision_id }
    })
  );
}
export async function activatePolicy(current: Policy) {
  ensureOk(
    await apiClient.POST('/api/v1/guardrail-policies/{policy_id}/activate', {
      params: {
        path: { policy_id: current.policy_id },
        header: {
          'If-Match': `"${current.etag}"`,
          'Idempotency-Key': crypto.randomUUID()
        }
      },
      body: { revision_id: current.revision_id }
    })
  );
}
export async function attachPolicy(
  body: components['schemas']['GuardrailAttachment']
) {
  ensureOk(
    await apiClient.POST('/api/v1/guardrail-attachments', {
      params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
      body
    })
  );
}
export async function evaluatePolicy(
  body: components['schemas']['GuardrailApplyInput']
) {
  return unwrap(await apiClient.POST('/api/v1/guardrails/evaluate', { body }));
}
