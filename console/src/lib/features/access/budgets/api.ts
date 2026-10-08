import { apiClient } from '$lib/api/client';
import { unwrap } from '$lib/api/http';
import type { components } from '$lib/api/schema';
export type AggregateBudget = components['schemas']['AggregateBudget'];
export type BudgetPolicy = components['schemas']['BudgetPolicy'];
export async function getBudget(
  projectId?: string,
  signal?: AbortSignal,
  organizationId?: string
): Promise<AggregateBudget> {
  if (organizationId)
    return unwrap(
      await apiClient.GET('/api/v1/organizations/{organization_id}/budget', {
        params: { path: { organization_id: organizationId } },
        signal
      })
    );
  return projectId
    ? unwrap(
        await apiClient.GET('/api/v1/projects/{project_id}/budget', {
          params: { path: { project_id: projectId } },
          signal
        })
      )
    : unwrap(await apiClient.GET('/api/v1/budgets/installation', { signal }));
}
export async function putBudget(
  projectId: string | undefined,
  etag: string,
  policy: BudgetPolicy | null,
  organizationId?: string
): Promise<AggregateBudget> {
  const headers = { 'If-Match': `"${etag}"` };
  if (organizationId)
    return unwrap(
      await apiClient.PUT('/api/v1/organizations/{organization_id}/budget', {
        params: { path: { organization_id: organizationId }, header: headers },
        body: { policy }
      })
    );
  return projectId
    ? unwrap(
        await apiClient.PUT('/api/v1/projects/{project_id}/budget', {
          params: { path: { project_id: projectId }, header: headers },
          body: { policy }
        })
      )
    : unwrap(
        await apiClient.PUT('/api/v1/budgets/installation', {
          params: { header: headers },
          body: { policy }
        })
      );
}
