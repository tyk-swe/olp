import { lifecycleNotice } from '#lib/features/providers/index.js';
import type {
  ProviderModelInventory,
  LifecycleNotice
} from '#lib/features/providers/index.js';

export type EditableTarget = {
  providerModelId: string;
  priority: number;
  weight: number;
  timeoutMs: number;
  tags?: string[];
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

export function storedTargetLabel(target: EditableTarget): string {
  if (target.stored?.providerModelId !== target.providerModelId)
    return 'Unknown model';
  return `${target.stored.providerName} · ${target.stored.providerModel}`;
}

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
  lifecycle: ProviderModelInventory['lifecycle'];
};

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

export function providerModel(
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

export type TargetLifecycleWarning = {
  label: string;
  notice: LifecycleNotice;
};

export function routeLifecycleWarnings(
  targets: EditableTarget[],
  modelOptions: RouteModelOption[],
  today: Date = new Date()
): TargetLifecycleWarning[] {
  return targets.flatMap((target) => {
    const option = providerModel(target, modelOptions);
    const notice = lifecycleNotice(option?.lifecycle, today);
    return option && notice ? [{ label: option.label, notice }] : [];
  });
}

export function retiringTargets(
  targets: { lifecycle?: ProviderModelInventory['lifecycle'] }[],
  today: Date = new Date()
): number {
  return targets.filter((target) => lifecycleNotice(target.lifecycle, today))
    .length;
}
