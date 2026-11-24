import type { QueryClient } from '@tanstack/svelte-query';
import { goto } from '$app/navigation';
import { resolve } from '$app/paths';
import { confirmAction } from '#lib/components/confirm.js';
import { overviewKeys } from '#lib/features/overview/index.js';
import { routeKeys } from '#lib/features/routes/routeKeys.js';
import { nativeObject, parseNativeJSON } from '#lib/nativeJson.js';
import type { components } from '#lib/management/schema.js';
import { errorMessage } from '#lib/management/http.js';
import type { MutationIntent } from '#lib/editing/mutationIntent.js';
import {
  acceptRemote,
  initialConcurrentEdit,
  markSaved,
  type ConcurrentEditState
} from '#lib/editing/concurrentEdit.js';
import {
  activateRoute,
  createRouteDraft,
  deleteRouteDraft,
  replaceRouteDraft,
  simulateRoute,
  validateRoute,
  type RouteActivation,
  type RouteDraft,
  type RouteSimulation
} from '#lib/features/routes/api.js';
import {
  buildCreateRouteDraftInput,
  buildReplaceRouteDraftInput,
  validateRouteEditor,
  type EditableTarget,
  type RouteBehavior,
  type RouteEditorValues,
  type RouteModelOption
} from '#lib/features/routes/routeEditor.js';

export const simulationNotice =
  'Deterministic attempt order calculated from the saved draft.';

export function hydrateRouteBehavior(current: RouteDraft): RouteBehavior {
  return {
    fallbacks: current.fallbacks,
    selectors: current.selectors,
    retry: current.retry,
    affinity: current.affinity,
    budget: current.budget,
    max_body_bytes: current.max_body_bytes,
    caller_cost_exempt: current.caller_cost_exempt,
    provider_fetched_documents: current.provider_fetched_documents,
    cache: current.cache,
    prompt_cache: current.prompt_cache,
    passthrough: current.passthrough,
    certification: current.certification,
    tool_execution: current.tool_execution,
    toolsets: current.toolsets ?? [],
    responses_mcp_items: current.responses_mcp_items ?? false,
    run_limits: current.run_limits ?? null
  };
}

export function hydrateRouteTargets(current: RouteDraft): EditableTarget[] {
  return current.targets.map((target) => ({
    providerModelId: target.provider_model_id,
    priority: target.priority,
    weight: target.weight,
    timeoutMs: target.timeout_ms,
    tags: [...target.tags],
    shadowSampleRate: target.shadow?.sample_rate ?? null,
    stored: {
      providerModelId: target.provider_model_id,
      available: target.available,
      providerName: target.provider_name,
      providerModel: target.provider_model
    }
  }));
}

export function parseSimulationRequest(
  source: string
): Record<string, unknown> | undefined {
  const trimmed = source.trim();
  if (!trimmed) return undefined;
  const parsed = parseNativeJSON(trimmed);
  if (!nativeObject(parsed))
    throw new Error('The native request must be a JSON object.');
  return parsed;
}

export type RouteDraftActionContext = {
  readonly queryClient: QueryClient;
  readonly createIntent: MutationIntent;
  readonly activateRouteIntent: MutationIntent;
  readonly canManage: () => boolean;
  readonly busy: () => string;
  readonly publicationBlocked: () => boolean;
  readonly createdDraftId: () => string;
  readonly setCreatedDraftId: (id: string) => void;
  readonly editorValues: () => RouteEditorValues;
  readonly modelOptions: () => RouteModelOption[];
  readonly sync: () => ConcurrentEditState;
  readonly setSync: (next: ConcurrentEditState) => void;
  readonly editVersion: () => number;
  readonly setError: (message: string) => void;
  readonly setNotice: (message: string) => void;
  readonly setSimulation: (value: RouteSimulation | null) => void;
  readonly setActivation: (value: RouteActivation | null) => void;
  readonly invalidateSimulation: () => void;
  readonly simulationVersion: () => number;
  readonly simulationParams: () => {
    operation: string;
    surface: string;
    mode: string;
    dialect: string;
    requestJson: string;
    seed: string;
    preferencesJson: string;
  };
  readonly run: (
    label: string,
    action: (
      isCurrent: () => boolean,
      samePrincipal: () => boolean
    ) => Promise<void>
  ) => Promise<void>;
};

export function createRouteDraftActions(ctx: RouteDraftActionContext) {
  return {
    create: async (event: SubmitEvent) => {
      event.preventDefault();
      if (!ctx.canManage() || ctx.busy()) return;
      const existingId = ctx.createdDraftId();
      if (existingId) {
        await ctx.run('open', async (isCurrent) => {
          if (isCurrent()) await goto(resolve(`routes/${existingId}`));
        });
        return;
      }
      const issue = validateRouteEditor(ctx.editorValues());
      if (issue) {
        ctx.setError(issue);
        return;
      }
      await ctx.run('save', async (isCurrent, samePrincipal) => {
        const { id } = await ctx.createIntent.submit(
          buildCreateRouteDraftInput(ctx.editorValues(), ctx.modelOptions()),
          (input, requestId) => createRouteDraft(input, requestId)
        );
        if (!samePrincipal()) return;
        if (isCurrent()) {
          ctx.setCreatedDraftId(id);
          ctx.setSync(initialConcurrentEdit());
          ctx.setNotice('Draft saved.');
        }
        try {
          await ctx.queryClient.invalidateQueries(
            { queryKey: routeKeys.lists },
            { throwOnError: true }
          );
        } catch (cause) {
          if (isCurrent())
            ctx.setError(
              `Draft saved, but refreshing failed. ${errorMessage(cause)}`
            );
        }
        if (isCurrent()) await goto(resolve(`routes/${id}`));
      });
    },

    save: async (current: RouteDraft) => {
      if (!ctx.canManage() || ctx.busy()) return;
      if (!ctx.sync().dirty) {
        ctx.setError('');
        ctx.setNotice('No unsaved changes.');
        return;
      }
      const issue = validateRouteEditor(ctx.editorValues());
      if (issue) {
        ctx.setError(issue);
        return;
      }
      ctx.invalidateSimulation();
      await ctx.run('save', async (isCurrent, samePrincipal) => {
        const snapshotEtag = ctx.sync().snapshotEtag;
        if (!snapshotEtag) throw new Error('Reload the draft before saving.');
        const submittedVersion = ctx.editVersion();
        const updated = await replaceRouteDraft(
          current.id,
          snapshotEtag,
          buildReplaceRouteDraftInput(ctx.editorValues())
        );
        if (!samePrincipal()) return;
        ctx.queryClient.setQueryData(routeKeys.draft(current.id), updated);
        if (!isCurrent()) return;
        const stillDirty = ctx.editVersion() !== submittedVersion;
        ctx.setSync(markSaved(updated.etag, stillDirty));
        ctx.setNotice(
          stillDirty
            ? 'Draft saved. You have additional unsaved changes.'
            : 'Draft saved. Validate to preview, or activate directly; activation validates the saved draft.'
        );
      });
    },

    simulate: async (current: RouteDraft) => {
      if (!ctx.canManage() || ctx.publicationBlocked()) return;
      ctx.invalidateSimulation();
      const version = ctx.simulationVersion();
      const params = ctx.simulationParams();
      await ctx.run('simulate', async (isCurrent) => {
        let simulation: RouteSimulation;
        try {
          simulation = await simulateRoute(current.id, {
            operation: params.operation,
            surface: params.surface,
            mode: params.mode,
            seed: params.seed || 'preview',
            preferences: JSON.parse(params.preferencesJson),
            request: parseSimulationRequest(params.requestJson),
            dialect: (params.dialect || undefined) as
              components['schemas']['SimulationDialect'] | undefined
          });
        } catch (error) {
          if (isCurrent() && version === ctx.simulationVersion()) throw error;
          return;
        }
        if (!isCurrent() || version !== ctx.simulationVersion()) return;
        ctx.setSimulation(simulation);
        ctx.setNotice(simulationNotice);
      });
    },

    validate: async (current: RouteDraft) => {
      if (!ctx.canManage() || ctx.publicationBlocked()) return;
      await ctx.run('validate', async (isCurrent, samePrincipal) => {
        const validation = await validateRoute(current);
        if (!samePrincipal()) return;
        ctx.queryClient.setQueryData<RouteDraft>(routeKeys.draft(current.id), {
          ...current,
          state: validation.state,
          etag: validation.etag
        });
        if (!isCurrent()) return;
        ctx.setSync(acceptRemote(ctx.sync(), validation.etag));
        ctx.setNotice(
          'Validation passed. The saved draft is ready to activate.'
        );
      });
    },

    activate: async (current: RouteDraft) => {
      if (!ctx.canManage() || ctx.publicationBlocked()) return;
      await ctx.run('activate', async (isCurrent, samePrincipal) => {
        const activation = await ctx.activateRouteIntent.submit(
          { id: current.id, etag: current.etag },
          (input, requestId) => activateRoute(input, requestId)
        );
        if (!samePrincipal()) return;
        if (isCurrent()) {
          ctx.setSync(acceptRemote(ctx.sync(), activation.draft_etag));
        }
        ctx.queryClient.setQueryData<RouteDraft>(routeKeys.draft(current.id), {
          ...current,
          state: 'draft',
          etag: activation.draft_etag
        });
        await Promise.all([
          ctx.queryClient.invalidateQueries({
            queryKey: routeKeys.draft(current.id)
          }),
          ctx.queryClient.invalidateQueries({ queryKey: routeKeys.lists }),
          ctx.queryClient.invalidateQueries({ queryKey: overviewKeys.root })
        ]);
        if (!isCurrent()) return;
        ctx.setActivation(activation);
        ctx.setNotice(
          `Route activated as revision ${activation.revision} in runtime generation ${activation.runtime_generation.sequence}.`
        );
      });
    },

    remove: async (current: RouteDraft) => {
      if (!ctx.canManage() || ctx.busy()) return;
      if (
        !(await confirmAction({
          title: `Delete draft ${current.slug}?`,
          body: 'The draft is removed. Live revisions are not affected.',
          confirmLabel: 'Delete draft',
          danger: true
        }))
      )
        return;
      await ctx.run('delete', async (isCurrent) => {
        await deleteRouteDraft(current.id, current.etag);
        await ctx.queryClient.invalidateQueries({
          queryKey: routeKeys.lists
        });
        if (!isCurrent()) return;
        ctx.setSync(initialConcurrentEdit());
        await goto(resolve('routes'));
      });
    }
  };
}
