import { validRouteSlug } from '#lib/routeIdentity.js';
import type { components } from '#lib/management/schema.js';
import type { RouteBehavior } from './routeBehavior';
import { validatePassthroughConfig } from './routePassthrough';
import type { EditableTarget } from './routeTargetModel';

export type RouteEditorValues = {
  slug: string;
  operations: string[];
  overallTimeoutMs: number;
  maxAttempts: number;
  targets: EditableTarget[];
  projectId?: string;
  /** Every draft the console saves states its fidelity; strict is the default. */
  fidelity: components['schemas']['RouteFidelity'];
  /** Fallbacks, selectors, retry policy, session affinity and spend cap. */
  behavior?: RouteBehavior;
  /** Why the selectors the operator typed cannot be read, if they cannot. */
  selectorsError?: string | null;
};

export function validateRouteEditor(values: RouteEditorValues): string | null {
  // Matches the server's route slug format.
  if (!validRouteSlug(values.slug)) {
    return 'Use 1–100 lowercase letters, digits, dots, underscores, or hyphens, starting with a letter or digit.';
  }
  if (!values.operations.length)
    return 'Select at least one supported operation.';
  if (!values.targets.length)
    return 'Add at least one eligible provider model target.';
  if (values.maxAttempts < 1 || values.maxAttempts > 32767) {
    return 'Maximum attempts must be between 1 and 32767; each credential attempt counts.';
  }
  if (
    !Number.isInteger(values.overallTimeoutMs) ||
    values.overallTimeoutMs < 1 ||
    values.overallTimeoutMs > 3600000
  ) {
    return 'Overall deadline must be from 1 to 3600000 ms.';
  }
  if (
    values.targets.some(
      (target) =>
        target.priority < 0 ||
        target.priority > 32767 ||
        target.weight < 1 ||
        target.weight > 1000000 ||
        target.timeoutMs < 1 ||
        target.timeoutMs > values.overallTimeoutMs
    )
  ) {
    return 'Every target needs a priority from 0 to 32767, a weight from 1 to 1000000, and a timeout from 1 ms up to the overall deadline.';
  }
  return validateTargetRoles(values.targets) ?? validateRouteBehavior(values);
}

const targetTag = /^[a-z0-9][a-z0-9_-]{0,31}$/;
const attributionLabel = /^[A-Za-z][A-Za-z0-9_.-]{0,31}$/;
const costLimit = /^(0|[1-9][0-9]{0,11})(\.[0-9]{1,12})?$/;

/** Tags and shadow sampling, matching the server's target rules. */
export function validateTargetRoles(targets: EditableTarget[]): string | null {
  for (const target of targets) {
    const tags = target.tags ?? [];
    if (
      tags.length > 8 ||
      new Set(tags).size !== tags.length ||
      tags.some((tag) => !targetTag.test(tag))
    )
      return 'Use up to 8 distinct tags of lowercase letters, digits, underscores or hyphens.';
    const rate = target.shadowSampleRate;
    if (rate != null && !(rate > 0 && rate <= 1))
      return 'A shadow target mirrors a share of requests greater than 0 and at most 1.';
  }
  if (
    targets.length &&
    targets.every((target) => target.shadowSampleRate != null)
  )
    return 'Declare at least one target that serves callers rather than shadowing them.';
  return null;
}

/** The route behavior rules the server enforces, checked before saving. */
export function validateRouteBehavior(
  values: RouteEditorValues
): string | null {
  if (values.selectorsError) return values.selectorsError;
  const behavior = values.behavior;
  if (!behavior) return null;
  return (
    validateToolExecution(behavior, values.fidelity) ??
    validatePassthroughRoute(values, behavior) ??
    validateFallbacks(behavior.fallbacks, values.slug) ??
    validateSelectors(behavior.selectors, values.targets) ??
    validateRetry(behavior.retry) ??
    validateAffinity(behavior.affinity) ??
    validateCaching(behavior, values.fidelity) ??
    validateBodyLimit(behavior.max_body_bytes) ??
    validateSpendCap(behavior.budget)
  );
}

function validateToolExecution(
  behavior: RouteBehavior,
  fidelity: RouteEditorValues['fidelity']
): string | null {
  if (behavior.tool_execution === 'gateway' && fidelity.mode !== 'transformed')
    return 'Gateway tool execution requires a transformed route.';
  return null;
}

function validatePassthroughRoute(
  values: RouteEditorValues,
  behavior: RouteBehavior
): string | null {
  const bindingProblem = validatePassthroughConfig(behavior.passthrough);
  if (bindingProblem) return bindingProblem;
  if (!behavior.passthrough)
    return values.operations.includes('passthrough')
      ? 'The passthrough operation requires a pass-through binding.'
      : null;
  if (values.operations.length !== 1 || values.operations[0] !== 'passthrough')
    return 'A pass-through route serves only the passthrough operation.';
  if (
    values.targets.length !== 1 ||
    values.targets[0]?.shadowSampleRate != null
  )
    return 'A pass-through route binds exactly one serving connection without mirroring.';
  if (values.maxAttempts !== 1)
    return 'A pass-through route makes exactly one attempt.';
  if (values.fidelity.mode === 'transformed')
    return 'A pass-through route cannot declare translation.';
  if (
    behavior.fallbacks.length ||
    behavior.selectors.length ||
    Object.keys(behavior.retry).length ||
    behavior.affinity ||
    behavior.cache ||
    behavior.prompt_cache
  )
    return 'A pass-through route cannot use fallback, selectors, retry, affinity or response caching.';
  return null;
}

function validateFallbacks(
  fallbacks: RouteBehavior['fallbacks'],
  slug: string
): string | null {
  if (fallbacks.length > 4) return 'Declare at most 4 fallback routes.';
  for (const fallback of fallbacks) {
    if (!validRouteSlug(fallback.route) || fallback.route === slug)
      return 'Every fallback names another route by its slug.';
    if (!fallback.on.length)
      return `Fallback “${fallback.route}” needs at least one condition.`;
  }
  return null;
}

function validateSelectors(
  selectors: RouteBehavior['selectors'],
  targets: EditableTarget[]
): string | null {
  const serving = new Set(
    targets
      .filter((target) => target.shadowSampleRate == null)
      .flatMap((target) => target.tags ?? [])
  );
  for (const selector of selectors) {
    if (!selector.route && !selector.tags?.length)
      return `Selector “${selector.id}” needs target tags or a route to delegate to.`;
    const missing = selector.tags?.find((tag) => !serving.has(tag));
    if (missing)
      return `Selector “${selector.id}” names tag “${missing}”, which no serving target carries.`;
  }
  return null;
}

function validateRetry(retry: RouteBehavior['retry']): string | null {
  for (const [kind, rule] of Object.entries(retry)) {
    if (
      !rule ||
      rule.max_retries < 0 ||
      rule.max_retries > 10 ||
      rule.base_backoff_ms < 0 ||
      rule.max_backoff_ms < rule.base_backoff_ms ||
      rule.max_backoff_ms > 60000
    )
      return `The ${kind} retry rule needs 0–10 retries and a backoff no longer than 60000 ms.`;
  }
  return null;
}

function validateAffinity(affinity: RouteBehavior['affinity']): string | null {
  if (
    affinity?.source === 'label' &&
    !attributionLabel.test(affinity.label ?? '')
  )
    return 'Session affinity by label names an attribution label.';
  return null;
}

function validateCaching(
  { cache, prompt_cache }: RouteBehavior,
  fidelity: RouteEditorValues['fidelity']
): string | null {
  if (
    fidelity.mode === 'strict' &&
    (cache?.mode === 'semantic' || prompt_cache)
  )
    return 'Semantic caching and prompt-cache insertion require a transformed route.';
  if (
    cache?.mode === 'semantic' &&
    (!cache.embedding_route || !cache.allow_unsealed_vectors)
  )
    return 'Semantic caching requires an embedding route and the unencrypted-vector acknowledgement.';
  return null;
}

function validateBodyLimit(
  cap: RouteBehavior['max_body_bytes']
): string | null {
  if (
    cap != null &&
    (!Number.isSafeInteger(cap) || cap < 1 || cap > 1073741824)
  )
    return 'Use a body limit from 1 to 1073741824 bytes, or leave it empty.';
  return null;
}

function validateSpendCap(budget: RouteBehavior['budget']): string | null {
  if (!budget) return null;
  const limits = [budget.daily_cost_limit, budget.monthly_cost_limit];
  if (limits.every((limit) => !limit))
    return 'A route spend cap declares a daily or monthly limit.';
  if (limits.some((limit) => limit && !costLimit.test(limit)))
    return 'Spend caps are decimal amounts with up to 12 decimal places.';
  return null;
}
