import type { components } from '$lib/api/schema';
import { apiClient } from '$lib/api/client';
import { result } from '$lib/api/http';

type Schemas = components['schemas'];
export type InspectRoutingInput = {
  route: string;
  operation: string;
  surface: Schemas['Surface'];
  mode: Schemas['TransportMode'];
  request?: Record<string, unknown>;
  dialect?: Schemas['SimulationDialect'];
  clientContract?: string;
  preferences?: Schemas['RoutingPreferences'];
  apiKeyId?: string | null;
  seed?: string;
};

/** Public management inspection. The backend plans against current authority
 * and the prepared request, and makes no provider or tool call. */
export async function inspectRouting(
  input: InspectRoutingInput,
  signal?: AbortSignal
): Promise<Schemas['RoutingDecision'][]> {
  const response = await apiClient.POST('/api/v1/routing/simulate', {
    body: {
      operation: {
        operation: input.operation,
        route: input.route,
        ...(input.request === undefined ? {} : { request: input.request })
      },
      surface: input.surface,
      mode: input.mode,
      preferences: input.preferences,
      api_key_id: input.apiKeyId ?? null,
      dialect: input.dialect,
      client_contract: input.clientContract,
      seed: input.seed ?? ''
    },
    signal
  });
  return result(response.data, response.error, response.response);
}
