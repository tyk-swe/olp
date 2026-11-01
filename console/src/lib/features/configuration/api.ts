import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { unwrap } from '$lib/api/http';

export type CodeBindings = components['schemas']['ConfigurationCodeBindings'];

export function isCodeBindings(value: unknown): value is CodeBindings {
  return (
    !!value &&
    typeof value === 'object' &&
    !Array.isArray(value) &&
    Object.entries(value).every(
      ([name, bindings]: [string, unknown]) =>
        ['credentials', 'users', 'api_keys'].includes(name) &&
        !!bindings &&
        typeof bindings === 'object' &&
        !Array.isArray(bindings) &&
        Object.values(bindings).every((id: unknown) => typeof id === 'string')
    )
  );
}

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
  externalBindings?: Record<string, ExternalCredentialReference>,
  codeBindings?: CodeBindings
): Promise<ConfigurationPlan> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/configuration/plan',
    {
      body: {
        document,
        secret_bindings: secretBindings,
        external_credential_bindings: externalBindings,
        code_bindings: codeBindings
      },
      signal
    }
  );
  return unwrap({ data, error, response });
}

export async function applyConfiguration(
  document: ConfigurationDocument,
  secretBindings: Record<string, string>,
  requestId: string,
  signal?: AbortSignal,
  externalBindings?: Record<string, ExternalCredentialReference>,
  codeBindings?: CodeBindings
): Promise<ConfigurationPlan> {
  const { data, error, response } = await apiClient.POST(
    '/api/v1/configuration/apply',
    {
      params: { header: { 'Idempotency-Key': requestId } },
      body: {
        document,
        secret_bindings: secretBindings,
        external_credential_bindings: externalBindings,
        code_bindings: codeBindings
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
