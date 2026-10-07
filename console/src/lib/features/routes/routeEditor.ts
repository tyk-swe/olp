import type { components } from '$lib/api/schema';
import type {
  CreateRouteDraftInput,
  ReplaceRouteDraftInput
} from '$lib/features/routes/api';
import type { ProviderModelInventory } from '$lib/features/providers/api/models';
import {
  lifecycleNotice,
  type LifecycleNotice
} from '$lib/features/providers/models/lifecycle';

export type EditableTarget = {
  providerModelId: string;
  priority: number;
  weight: number;
  timeoutMs: number;
  /** Tags route selectors narrow the route's targets by. */
  tags?: string[];
  /** The share of requests mirrored to the target, or null when it serves. */
  shadowSampleRate?: number | null;
  /**
   * Facts the API reported for a target the draft already stores. A stored
   * target stays in the draft after its provider is disabled or its model
   * leaves the provider's activated revision, and then it is no longer in the
   * enabled-model inventory the picker offers — so the editor keeps its own
   * copy of the identity to render instead of dropping it. Absent on rows the
   * operator has just added.
   */
  stored?: {
    providerModelId: string;
    available: boolean;
    providerName: string;
    providerModel: string;
  };
};

/** Label for a stored target the enabled-model inventory no longer offers. */
export function storedTargetLabel(target: EditableTarget): string {
  if (target.stored?.providerModelId !== target.providerModelId)
    return 'Unknown model';
  return `${target.stored.providerName} · ${target.stored.providerModel}`;
}

/** True when the API flagged the stored target as no longer routable. */
export function targetUnavailable(target: EditableTarget): boolean {
  return (
    target.stored?.providerModelId === target.providerModelId &&
    target.stored.available === false
  );
}

export type RouteModelOption = {
  id: string;
  providerId: string;
  providerName: string;
  upstreamModel: string;
  label: string;
  capabilities: ProviderModelInventory['model']['capabilities'];
  /** The model's documented deprecation and retirement, or null. */
  lifecycle: ProviderModelInventory['lifecycle'];
};

export type EditablePolicyRule = {
  id: string;
  phase: 'input' | 'output';
  action: 'block' | 'redact';
  pattern: string;
  replacement: string;
};

export type ContentPolicy = components['schemas']['ContentPolicy'];
export type RouteBehavior = components['schemas']['RouteBehavior'];
export type RouteSelector = components['schemas']['RouteSelector'];
export type FallbackCondition = components['schemas']['FallbackCondition'];
export type RetryClass =
  'connect' | 'timeout' | 'rate_limit' | 'upstream_server';

export const fallbackConditions: [FallbackCondition, string][] = [
  ['exhausted', 'Every attempt failed'],
  ['context_window', 'Context window exceeded'],
  ['content_filter', 'Content filter refusal'],
  ['rate_limit', 'Rate limited'],
  ['budget', 'Spend cap exhausted']
];

export const retryClasses: [RetryClass, string][] = [
  ['connect', 'Connection failures'],
  ['timeout', 'Timeouts'],
  ['rate_limit', 'Rate limits'],
  ['upstream_server', 'Upstream server errors']
];

export function emptyBehavior(): RouteBehavior {
  return {
    fallbacks: [],
    selectors: [],
    retry: {},
    affinity: null,
    budget: null
  };
}

export type RouteEditorValues = {
  slug: string;
  operations: string[];
  overallTimeoutMs: number;
  maxAttempts: number;
  targets: EditableTarget[];
  contentPolicyRules: EditablePolicyRule[];
  projectId?: string;
  /** Every draft the console saves states its fidelity; strict is the default. */
  fidelity: components['schemas']['RouteFidelity'];
  /** Fallbacks, selectors, retry policy, session affinity and spend cap. */
  behavior?: RouteBehavior;
  /** Why the selectors the operator typed cannot be read, if they cannot. */
  selectorsError?: string | null;
};

/** Reads selectors typed as JSON, or explains why they cannot be read. */
export function parseSelectors(text: string): RouteSelector[] | string {
  if (!text.trim()) return [];
  try {
    const parsed: unknown = JSON.parse(text);
    if (Array.isArray(parsed) && parsed.every(isSelector)) return parsed;
  } catch {
    // Reported below.
  }
  return 'Selectors must be a JSON array of selector objects.';
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isStringArray(value: unknown): value is string[] {
  return (
    Array.isArray(value) && value.every((item) => typeof item === 'string')
  );
}

function isSelector(value: unknown): value is RouteSelector {
  if (
    !isObject(value) ||
    typeof value.id !== 'string' ||
    !isObject(value.when) ||
    (value.route !== undefined && typeof value.route !== 'string') ||
    (value.tags !== undefined && !isStringArray(value.tags)) ||
    Object.keys(value).some(
      (key) => !['id', 'when', 'route', 'tags'].includes(key)
    )
  )
    return false;
  return Object.entries(value.when).every(([key, field]) => {
    switch (key) {
      case 'operations':
      case 'modalities':
      case 'reasoning_effort':
        return isStringArray(field);
      case 'min_input_tokens':
      case 'max_input_tokens':
      case 'min_output_tokens':
      case 'max_output_tokens':
        return field === null || Number.isSafeInteger(field);
      case 'streaming':
      case 'tools':
      case 'structured_output':
        return field === null || typeof field === 'boolean';
      case 'classifier':
        return (
          field === null ||
          (isObject(field) &&
            typeof field.route === 'string' &&
            isStringArray(field.labels) &&
            Number.isSafeInteger(field.timeout_ms) &&
            (field.min_score == null || typeof field.min_score === 'number') &&
            Object.keys(field).every((name) =>
              ['route', 'labels', 'timeout_ms', 'min_score'].includes(name)
            ))
        );
      case 'plugin':
        return (
          field === null ||
          (isObject(field) &&
            typeof field.digest === 'string' &&
            Object.keys(field).every((name) => name === 'digest'))
        );
      default:
        return false;
    }
  });
}

/** Omitted modes are strict, matching the server's default. */
export function fidelityLabel(
  fidelity?: components['schemas']['RouteFidelity'] | null
): 'Strict' | 'Transformed' {
  return fidelity?.mode === 'transformed' ? 'Transformed' : 'Strict';
}

export const operationOptions = [
  ['generation', 'Text generation'],
  ['embeddings', 'Embeddings'],
  ['rerank', 'Rerank'],
  ['token_count', 'Token counting'],
  ['image_generation', 'Image generation'],
  ['image_edit', 'Image editing'],
  ['image_variation', 'Image variations'],
  ['speech', 'Speech'],
  ['transcription', 'Transcription'],
  ['translation', 'Audio translation'],
  ['video_create', 'Create video'],
  ['video_list', 'List videos'],
  ['video_get', 'Video status'],
  ['video_content', 'Video content'],
  ['video_delete', 'Delete video'],
  ['moderation', 'Moderation'],
  ['batch', 'Batches'],
  ['realtime', 'Realtime sessions'],
  ['bedrock_invoke', 'Bedrock InvokeModel']
] as const;

export function toRouteModelOptions(
  inventory: ProviderModelInventory[]
): RouteModelOption[] {
  return inventory.map((entry) => ({
    id: entry.model.id,
    providerId: entry.provider_id,
    providerName: entry.provider_name,
    upstreamModel: entry.model.upstream_model,
    label: `${entry.provider_name} · ${entry.model.display_name}`,
    capabilities: entry.model.capabilities,
    lifecycle: entry.lifecycle ?? null
  }));
}

export function surfacesFor(operation: string): string[] {
  if (operation === 'bedrock_invoke') return ['bedrock'];
  if (operation === 'generation')
    return ['openai', 'anthropic', 'gemini', 'bedrock'];
  if (operation === 'token_count') return ['openai', 'anthropic', 'gemini'];
  return ['openai'];
}

export function modesFor(operation: string): string[] {
  if (operation === 'video_create') return ['async'];
  if (operation === 'realtime') return ['realtime'];
  if (
    [
      'generation',
      'image_generation',
      'image_edit',
      'speech',
      'transcription',
      'bedrock_invoke'
    ].includes(operation)
  ) {
    return ['unary', 'streaming'];
  }
  return ['unary'];
}

function providerModel(
  target: EditableTarget,
  modelOptions: RouteModelOption[]
) {
  return modelOptions.find((option) => option.id === target.providerModelId);
}

export function certifiedCapabilities(
  target: EditableTarget,
  modelOptions: RouteModelOption[],
  operations: string[]
) {
  return (providerModel(target, modelOptions)?.capabilities ?? []).filter(
    (capability) =>
      capability.source === 'certified' &&
      operations.includes(capability.operation)
  );
}

export function missingTargetOperations(
  target: EditableTarget,
  modelOptions: RouteModelOption[],
  operations: string[]
): string[] {
  const capabilities = certifiedCapabilities(target, modelOptions, operations);
  return operations.filter(
    (operation) =>
      !capabilities.some((capability) => capability.operation === operation)
  );
}

export function eligibleTargetTuples(
  target: EditableTarget,
  modelOptions: RouteModelOption[],
  operations: string[]
): string[] {
  return certifiedCapabilities(target, modelOptions, operations).map(
    (capability) =>
      `${capability.operation} · ${capability.surface} · ${capability.mode}`
  );
}

export function routeEligibilityWarnings(
  targets: EditableTarget[],
  modelOptions: RouteModelOption[],
  operations: string[]
): string[] {
  return operations.filter(
    (operation) =>
      !targets.some((target) =>
        certifiedCapabilities(target, modelOptions, operations).some(
          (capability) => capability.operation === operation
        )
      )
  );
}

/** A target whose model its vendor has deprecated or is retiring. */
export type TargetLifecycleWarning = {
  label: string;
  notice: LifecycleNotice;
};

/** The selected targets whose models need a lifecycle warning. */
export function routeLifecycleWarnings(
  targets: EditableTarget[],
  modelOptions: RouteModelOption[],
  today: Date = new Date()
): TargetLifecycleWarning[] {
  const warnings: TargetLifecycleWarning[] = [];
  for (const target of targets) {
    const option = modelOptions.find(
      (candidate) => candidate.id === target.providerModelId
    );
    const notice = lifecycleNotice(option?.lifecycle, today);
    if (option && notice) warnings.push({ label: option.label, notice });
  }
  return warnings;
}

/** How many of a route's targets have a model deprecated or retiring soon. */
export function retiringTargets(
  targets: { lifecycle?: ProviderModelInventory['lifecycle'] }[],
  today: Date = new Date()
): number {
  return targets.filter((target) => lifecycleNotice(target.lifecycle, today))
    .length;
}

const policyRuleId = /^[A-Za-z][A-Za-z0-9_-]{0,63}$/;
const POLICY_MAX_RULES = 64;
const POLICY_MAX_PATTERN_BYTES = 512;
const POLICY_MAX_PATTERN_TOTAL_BYTES = 16 * 1024;
const POLICY_MAX_REPLACEMENT_CHARS = 128;

export function policyRulesFrom(
  policy: ContentPolicy | null | undefined
): EditablePolicyRule[] {
  return (policy?.rules ?? []).map((rule) => ({
    id: rule.id,
    phase: rule.phase,
    action: rule.action,
    pattern: rule.pattern,
    replacement: rule.replacement ?? ''
  }));
}

export function hasOutputRules(rules: { phase: string }[]): boolean {
  return rules.some((rule) => rule.phase === 'output');
}

export function validateContentPolicy(
  rules: EditablePolicyRule[]
): string | null {
  if (rules.length > POLICY_MAX_RULES)
    return `At most ${POLICY_MAX_RULES} content policy rules are allowed.`;
  const seen = new Set<string>();
  const encoder = new TextEncoder();
  let patternBytes = 0;
  for (const rule of rules) {
    if (!policyRuleId.test(rule.id))
      return `Rule id “${rule.id || '?'}” must start with a letter and use at most 64 letters, digits, underscores, or hyphens.`;
    if (seen.has(rule.id))
      return `Rule id “${rule.id}” is used more than once.`;
    seen.add(rule.id);
    const patternLength = encoder.encode(rule.pattern).length;
    if (patternLength < 1 || patternLength > POLICY_MAX_PATTERN_BYTES)
      return `Rule “${rule.id}” needs a RE2 pattern of 1–${POLICY_MAX_PATTERN_BYTES} bytes.`;
    patternBytes += patternLength;
    if (
      rule.action === 'redact' &&
      [...rule.replacement].length > POLICY_MAX_REPLACEMENT_CHARS
    )
      return `Rule “${rule.id}” replacement is limited to ${POLICY_MAX_REPLACEMENT_CHARS} characters.`;
  }
  if (patternBytes > POLICY_MAX_PATTERN_TOTAL_BYTES)
    return `Content policy patterns may not exceed ${POLICY_MAX_PATTERN_TOTAL_BYTES} bytes combined.`;
  return null;
}

export function buildContentPolicy(
  rules: EditablePolicyRule[]
): ContentPolicy | null {
  if (!rules.length) return null;
  return {
    rules: rules.map((rule) => ({
      id: rule.id,
      phase: rule.phase,
      pattern: rule.pattern,
      action: rule.action,
      ...(rule.action === 'redact' && rule.replacement
        ? { replacement: rule.replacement }
        : {})
    }))
  };
}

export function validateRouteEditor(values: RouteEditorValues): string | null {
  // Matches the server's route slug format.
  if (!/^[a-z0-9][a-z0-9._-]{0,99}$/.test(values.slug)) {
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
  return (
    validateTargetRoles(values.targets) ??
    validateRouteBehavior(values) ??
    validateContentPolicy(values.contentPolicyRules)
  );
}

const targetTag = /^[a-z0-9][a-z0-9_-]{0,31}$/;
const routeSlug = /^[a-z0-9][a-z0-9._-]{0,99}$/;
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
  if (behavior.fallbacks.length > 4)
    return 'Declare at most 4 fallback routes.';
  for (const fallback of behavior.fallbacks) {
    if (!routeSlug.test(fallback.route) || fallback.route === values.slug)
      return 'Every fallback names another route by its slug.';
    if (!fallback.on.length)
      return `Fallback “${fallback.route}” needs at least one condition.`;
  }
  const serving = new Set(
    values.targets
      .filter((target) => target.shadowSampleRate == null)
      .flatMap((target) => target.tags ?? [])
  );
  for (const selector of behavior.selectors) {
    if (!selector.route && !selector.tags?.length)
      return `Selector “${selector.id}” needs target tags or a route to delegate to.`;
    const missing = selector.tags?.find((tag) => !serving.has(tag));
    if (missing)
      return `Selector “${selector.id}” names tag “${missing}”, which no serving target carries.`;
  }
  for (const [kind, rule] of Object.entries(behavior.retry)) {
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
  if (
    behavior.affinity?.source === 'label' &&
    !attributionLabel.test(behavior.affinity.label ?? '')
  )
    return 'Session affinity by label names an attribution label.';
  const budget = behavior.budget;
  if (budget) {
    const limits = [budget.daily_cost_limit, budget.monthly_cost_limit];
    if (limits.every((limit) => !limit))
      return 'A route spend cap declares a daily or monthly limit.';
    if (limits.some((limit) => limit && !costLimit.test(limit)))
      return 'Spend caps are decimal amounts with up to 12 decimal places.';
  }
  return null;
}

/** The behavior fields a draft write carries. */
function behaviorFields(behavior: RouteBehavior | undefined) {
  const value = behavior ?? emptyBehavior();
  return {
    fallbacks: value.fallbacks,
    selectors: value.selectors,
    retry: value.retry,
    affinity: value.affinity,
    budget: value.budget
  };
}

/** The tag and shadow fields one target write carries. */
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
    content_policy: buildContentPolicy(values.contentPolicyRules),
    fidelity: values.fidelity,
    ...behaviorFields(values.behavior),
    targets: values.targets.map((target) => {
      const model = providerModel(target, modelOptions)!;
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
    content_policy: buildContentPolicy(values.contentPolicyRules),
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
