import type { RouteDraft } from '#lib/features/routes/api.js';
import type { RouteBehavior } from '#lib/features/routes/routeBehavior.js';
import type { EditableTarget } from '#lib/features/routes/routeTargetModel.js';

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
