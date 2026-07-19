import type {
  CreateRouteDraftInput,
  ReplaceRouteDraftInput
} from '$lib/api/management/routes';
import type { ProviderModelInventory } from '$lib/api/management/providers';

export type EditableTarget = {
  providerModelId: string;
  priority: number;
  weight: number;
  timeoutMs: number;
};

export type RouteModelOption = {
  id: string;
  providerId: string;
  providerName: string;
  upstreamModel: string;
  label: string;
  capabilities: ProviderModelInventory['model']['capabilities'];
};

export type RouteEditorValues = {
  slug: string;
  operations: string[];
  overallTimeoutMs: number;
  maxAttempts: number;
  targets: EditableTarget[];
};

export const operationOptions = [
  ['generation', 'Text generation'],
  ['embeddings', 'Embeddings'],
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
  ['moderation', 'Moderation']
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
  return ['generation', 'token_count'].includes(operation)
    ? ['open_ai', 'anthropic', 'gemini']
    : ['open_ai'];
}

export function modesFor(operation: string): string[] {
  if (operation === 'video_create') return ['async'];
  if (
    ['generation', 'image_generation', 'image_edit', 'speech', 'transcription'].includes(
      operation
    )
  ) {
    return ['unary', 'streaming'];
  }
  return ['unary'];
}

function providerModel(target: EditableTarget, modelOptions: RouteModelOption[]) {
  return modelOptions.find((option) => option.id === target.providerModelId);
}

export function certifiedCapabilities(
  target: EditableTarget,
  modelOptions: RouteModelOption[],
  operations: string[]
) {
  return (providerModel(target, modelOptions)?.capabilities ?? []).filter(
    (capability) =>
      capability.source === 'certified' && operations.includes(capability.operation)
  );
}

export function missingTargetOperations(
  target: EditableTarget,
  modelOptions: RouteModelOption[],
  operations: string[]
): string[] {
  const capabilities = certifiedCapabilities(target, modelOptions, operations);
  return operations.filter(
    (operation) => !capabilities.some((capability) => capability.operation === operation)
  );
}

export function eligibleTargetTuples(
  target: EditableTarget,
  modelOptions: RouteModelOption[],
  operations: string[]
): string[] {
  return certifiedCapabilities(target, modelOptions, operations).map(
    (capability) => `${capability.operation} · ${capability.surface} · ${capability.mode}`
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

export function validateRouteEditor(values: RouteEditorValues): string | null {
  const validSlug =
    /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(values.slug) &&
    !values.slug.includes('--');
  if (!validSlug) {
    return 'Use 1–63 lowercase letters or numbers with single internal hyphens.';
  }
  if (!values.operations.length) return 'Select at least one supported operation.';
  if (!values.targets.length) return 'Add at least one eligible provider model target.';
  if (values.maxAttempts < 1 || values.maxAttempts > values.targets.length) {
    return 'Maximum attempts must be between 1 and the number of targets.';
  }
  if (
    values.targets.some(
      (target) => target.weight < 1 || target.timeoutMs < 100 || target.priority < 1
    )
  ) {
    return 'Every target needs a positive priority, weight, and timeout.';
  }
  return null;
}

export function buildCreateRouteDraftInput(
  values: RouteEditorValues,
  modelOptions: RouteModelOption[]
): CreateRouteDraftInput {
  return {
    slug: values.slug,
    operations: values.operations,
    overall_timeout_ms: values.overallTimeoutMs,
    max_attempts: values.maxAttempts,
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
    targets: values.targets.map((target) => ({
      provider_model_id: target.providerModelId,
      priority: target.priority,
      weight: target.weight,
      timeout_ms: target.timeoutMs
    }))
  };
}
