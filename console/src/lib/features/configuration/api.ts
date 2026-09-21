import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { result } from '$lib/api/http';

export type ConfigurationDocument =
  components['schemas']['ConfigurationDocument'];
export type ConfigurationPlan =
  components['schemas']['ConfigurationPlanResponse'];
export type ConfigurationPlanItem =
  components['schemas']['ConfigurationPlanItem'];
export type ConfigurationExport =
  components['schemas']['ConfigurationExportResponse'];

export async function exportConfiguration(
  signal?: AbortSignal
): Promise<ConfigurationExport> {
  const { data, error, response } = await apiClient.GET(
    '/api/v3/configuration/export',
    { signal }
  );
  return result(data, error, response);
}

export async function planConfiguration(
  document: ConfigurationDocument,
  secretBindings: Record<string, string>,
  signal?: AbortSignal
): Promise<ConfigurationPlan> {
  const { data, error, response } = await apiClient.POST(
    '/api/v3/configuration/plan',
    { body: { document, secret_bindings: secretBindings }, signal }
  );
  return result(data, error, response);
}

export async function applyConfiguration(
  document: ConfigurationDocument,
  secretBindings: Record<string, string>,
  signal?: AbortSignal
): Promise<ConfigurationPlan> {
  const { data, error, response } = await apiClient.POST(
    '/api/v3/configuration/apply',
    {
      params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
      body: { document, secret_bindings: secretBindings },
      signal
    }
  );
  return result(data, error, response);
}

export function missingSecretBindings(plan: ConfigurationPlan): string[] {
  return plan.blockers
    .filter((item) => item.detail === 'secret_binding_required')
    .map((item) => item.key);
}
