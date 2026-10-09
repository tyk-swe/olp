import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { unwrap } from '$lib/api/http';

export type ConfigurationDocument =
  components['schemas']['ConfigurationDocument'];
export type ConfigurationPlan =
  components['schemas']['ConfigurationPlanResponse'];
export type ConfigurationPlanItem =
  components['schemas']['ConfigurationPlanItem'];
export type ConfigurationExport =
  components['schemas']['ConfigurationExportResponse'];
export type ExternalCredentialReference =
  components['schemas']['ExternalCredentialReference'];

export async function exportConfiguration(
  signal?: AbortSignal
): Promise<ConfigurationExport> {
  const { data, error, response } = await apiClient.GET(
    '/api/v1/configuration/export',
    { signal }
  );
  return unwrap({ data, error, response });
}

export async function planConfiguration(
  document: ConfigurationDocument,
  secretBindings: Record<string, string>,
  signal?: AbortSignal,
  externalBindings?: Record<string, ExternalCredentialReference>
): Promise<ConfigurationPlan> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/configuration/plan',
    {
      body: {
        document,
        secret_bindings: secretBindings,
        external_credential_bindings: externalBindings
      },
      signal
    }
  );
  return unwrap({ data, error, response });
}

export async function applyConfiguration(
  document: ConfigurationDocument,
  secretBindings: Record<string, string>,
  signal?: AbortSignal,
  externalBindings?: Record<string, ExternalCredentialReference>
): Promise<ConfigurationPlan> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/configuration/apply',
    {
      params: { header: { 'Idempotency-Key': crypto.randomUUID() } },
      body: {
        document,
        secret_bindings: secretBindings,
        external_credential_bindings: externalBindings
      },
      signal
    }
  );
  return unwrap({ data, error, response });
}

export function missingSecretBindings(plan: ConfigurationPlan): string[] {
  return plan.blockers
    .filter((item) => item.detail === 'secret_binding_required')
    .map((item) => item.key);
}

/** The credential references a plan leaves for grant enrollment after
 * applying: exports never carry grants. */
export function grantEnrollments(plan: ConfigurationPlan): string[] {
  return plan.actions
    .filter((item) => item.action === 'enroll')
    .map((item) => item.key);
}

/** The names of the artifact's providers that pin the plugin build with the
 * digest. */
export function pinningProviders(
  document: ConfigurationDocument,
  digest: string
): string[] {
  return document.providers
    .filter(
      (provider) =>
        provider.configuration.kind === 'plugin' &&
        provider.configuration.profile_revision === digest
    )
    .map((provider) => provider.name);
}
