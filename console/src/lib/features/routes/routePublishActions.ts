import { overviewKeys } from '#lib/features/overview/index.js';
import { routeKeys } from '#lib/domain/routes/routeKeys.js';
import { nativeObject, parseNativeJSON } from '#lib/nativeJson.js';
import type { MutationIntent } from '#lib/editing/mutationIntent.js';
import { acceptRemote } from '#lib/editing/concurrentEdit.js';
import {
  activateRoute,
  simulateRoute,
  validateRoute,
  type RouteActivation,
  type RouteDraft,
  type RouteSimulation,
  type RouteSimulationInput
} from '#lib/features/routes/api.js';
import type { RouteDraftPort } from './routeDraftActions';

export const simulationNotice =
  'Deterministic attempt order calculated from the saved draft.';

export type SimulationForm = {
  readonly operation: string;
  readonly surface: string;
  readonly mode: string;
  readonly dialect: string;
  readonly requestJson: string;
  readonly seed: string;
  readonly preferencesJson: string;
};

function parseSimulationRequest(
  source: string
): Record<string, unknown> | undefined {
  const trimmed = source.trim();
  if (!trimmed) return undefined;
  const parsed = parseNativeJSON(trimmed);
  if (!nativeObject(parsed))
    throw new Error('The native request must be a JSON object.');
  return parsed;
}

function simulationInput(form: SimulationForm): RouteSimulationInput {
  return {
    operation: form.operation,
    surface: form.surface,
    mode: form.mode,
    seed: form.seed || 'preview',
    preferences: JSON.parse(form.preferencesJson),
    request: parseSimulationRequest(form.requestJson),
    dialect: (form.dialect || undefined) as RouteSimulationInput['dialect']
  };
}

export function simulateDraft(
  port: RouteDraftPort,
  current: RouteDraft,
  form: SimulationForm,
  isLatest: () => boolean,
  onSimulated: (simulation: RouteSimulation) => void
) {
  return port.run('simulate', async (isCurrent) => {
    const input = simulationInput(form);
    const simulation = await simulateRoute(current.id, input).catch(
      (error: unknown) => {
        if (isCurrent() && isLatest()) throw error;
        return null;
      }
    );
    if (!simulation || !isCurrent() || !isLatest()) return;
    onSimulated(simulation);
    port.setNotice(simulationNotice);
  });
}

export function validateDraft(port: RouteDraftPort, current: RouteDraft) {
  return port.run('validate', async (isCurrent, samePrincipal) => {
    const validation = await validateRoute(current);
    if (!samePrincipal()) return;
    port.queryClient.setQueryData<RouteDraft>(routeKeys.draft(current.id), {
      ...current,
      state: validation.state,
      etag: validation.etag
    });
    if (!isCurrent()) return;
    port.setSync(acceptRemote(port.sync(), validation.etag));
    port.setNotice('Validation passed. The saved draft is ready to activate.');
  });
}

export function activateDraft(
  port: RouteDraftPort,
  current: RouteDraft,
  intent: MutationIntent,
  onActivated: (activation: RouteActivation) => void
) {
  return port.run('activate', async (isCurrent, samePrincipal) => {
    const activation = await intent.submit(
      { id: current.id, etag: current.etag },
      (input, requestId) => activateRoute(input, requestId)
    );
    if (!samePrincipal()) return;
    if (isCurrent()) {
      port.setSync(acceptRemote(port.sync(), activation.draft_etag));
    }
    port.queryClient.setQueryData<RouteDraft>(routeKeys.draft(current.id), {
      ...current,
      state: 'draft',
      etag: activation.draft_etag
    });
    await Promise.all([
      port.queryClient.invalidateQueries({
        queryKey: routeKeys.draft(current.id)
      }),
      port.queryClient.invalidateQueries({ queryKey: routeKeys.lists }),
      port.queryClient.invalidateQueries({ queryKey: overviewKeys.root })
    ]);
    if (!isCurrent()) return;
    onActivated(activation);
    port.setNotice(
      `Route activated as revision ${activation.revision} in runtime generation ${activation.runtime_generation.sequence}.`
    );
  });
}
