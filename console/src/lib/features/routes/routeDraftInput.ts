import type {
  CreateRouteDraftInput,
  ReplaceRouteDraftInput
} from '#lib/features/routes/api.js';
import { emptyBehavior, type RouteBehavior } from './routeBehavior';
import type { RouteEditorValues } from './routeEditor';
import {
  providerModel,
  type EditableTarget,
  type RouteModelOption
} from './routeTargetModel';

function behaviorFields(behavior: RouteBehavior | undefined) {
  const value = behavior ?? emptyBehavior();
  return {
    fallbacks: value.fallbacks,
    selectors: value.selectors,
    retry: value.retry,
    affinity: value.affinity,
    budget: value.budget,
    cache: value.cache,
    prompt_cache: value.prompt_cache,
    tool_execution: value.tool_execution,
    toolsets: value.toolsets,
    responses_mcp_items: value.responses_mcp_items,
    run_limits: value.run_limits,
    passthrough: value.passthrough ?? null,
    ...(value.caller_cost_exempt !== undefined
      ? { caller_cost_exempt: value.caller_cost_exempt }
      : {}),
    ...(value.provider_fetched_documents !== undefined
      ? { provider_fetched_documents: value.provider_fetched_documents }
      : {}),
    ...(value.max_body_bytes !== undefined
      ? { max_body_bytes: value.max_body_bytes }
      : {})
  };
}

function roleFields(target: EditableTarget) {
  return {
    tags: target.tags ?? [],
    shadow:
      target.shadowSampleRate == null
        ? null
        : { sample_rate: target.shadowSampleRate }
  };
}

export function buildCreateRouteDraftInput(
  values: RouteEditorValues,
  modelOptions: RouteModelOption[]
): CreateRouteDraftInput {
  return {
    slug: values.slug,
    project_id: values.projectId || null,
    operations: values.operations,
    overall_timeout_ms: values.overallTimeoutMs,
    max_attempts: values.maxAttempts,
    fidelity: values.fidelity,
    ...behaviorFields(values.behavior),
    targets: values.targets.map((target) => {
      const model = providerModel(target, modelOptions);
      if (!model)
        throw new Error(
          `Route target ${target.providerModelId} is not in the provider model inventory.`
        );
      return {
        provider_id: model.providerId,
        provider_model: model.upstreamModel,
        priority: target.priority,
        weight: target.weight,
        timeout_ms: target.timeoutMs,
        ...roleFields(target)
      };
    })
  };
}

export function buildReplaceRouteDraftInput(
  values: RouteEditorValues
): ReplaceRouteDraftInput {
  return {
    slug: values.slug,
    operations: values.operations,
    overall_timeout_ms: values.overallTimeoutMs,
    max_attempts: values.maxAttempts,
    fidelity: values.fidelity,
    ...behaviorFields(values.behavior),
    targets: values.targets.map((target) => ({
      provider_model_id: target.providerModelId,
      priority: target.priority,
      weight: target.weight,
      timeout_ms: target.timeoutMs,
      ...roleFields(target)
    }))
  };
}
