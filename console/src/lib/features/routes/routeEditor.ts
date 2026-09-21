import type { components } from '$lib/api/schema';
import type {
  CreateRouteDraftInput,
  ReplaceRouteDraftInput
} from '$lib/features/routes/api';
import type { ProviderModelInventory } from '$lib/features/providers/models';

export type EditableTarget = {
  providerModelId: string;
  priority: number;
  weight: number;
  timeoutMs: number;
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
};

export type EditablePolicyRule = {
  id: string;
  phase: 'input' | 'output';
  action: 'block' | 'redact';
  pattern: string;
  replacement: string;
};

export type ContentPolicy = components['schemas']['ContentPolicy'];

export type RouteEditorValues = {
  slug: string;
  operations: string[];
  overallTimeoutMs: number;
  maxAttempts: number;
  targets: EditableTarget[];
  contentPolicyRules: EditablePolicyRule[];
  projectId?: string;
};

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
    capabilities: entry.model.capabilities
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
    if (rule.action === 'block' && rule.replacement)
      return `Rule “${rule.id}” blocks, so it cannot carry a replacement.`;
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
  const validSlug =
    /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(values.slug) &&
    !values.slug.includes('--');
  if (!validSlug) {
    return 'Use 1–63 lowercase letters or numbers with single internal hyphens.';
  }
  if (!values.operations.length)
    return 'Select at least one supported operation.';
  if (!values.targets.length)
    return 'Add at least one eligible provider model target.';
  if (values.maxAttempts < 1 || values.maxAttempts > 32767) {
    return 'Maximum attempts must be between 1 and 32767; each credential attempt counts.';
  }
  if (
    values.targets.some(
      (target) =>
        target.weight < 1 ||
        target.timeoutMs < 100 ||
        target.priority < 0 ||
        target.priority > 65535
    )
  ) {
    return 'Every target needs a priority from 0 to 65535, a positive weight, and a timeout of at least 100 ms.';
  }
  return validateContentPolicy(values.contentPolicyRules);
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
    targets: values.targets.map((target) => {
      const model = providerModel(target, modelOptions)!;
      return {
        provider_id: model.providerId,
        provider_model: model.upstreamModel,
        priority: target.priority,
        weight: target.weight,
        timeout_ms: target.timeoutMs
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
    targets: values.targets.map((target) => ({
      provider_model_id: target.providerModelId,
      priority: target.priority,
      weight: target.weight,
      timeout_ms: target.timeoutMs
    }))
  };
}
