import type { components } from '#lib/management/schema.js';

export type FidelityMode = components['schemas']['RouteFidelity']['mode'];

const modeLabels: Record<FidelityMode, string> = {
  strict: 'Strict · preserve the native invocation',
  transformed: 'Transformed · translate or redact deliberately'
};

/** Route fidelity choices. Routes are strict unless declared transformed. */
export const fidelityOptions = (Object.keys(modeLabels) as FidelityMode[]).map(
  (value) => ({ value, label: modeLabels[value] })
);

export function fidelityLabel(
  fidelity?: components['schemas']['RouteFidelity'] | null,
  passthrough?: components['schemas']['PassthroughConfig'] | null
): 'Strict' | 'Transformed' | 'Pass-through · uncertified' {
  if (passthrough) return 'Pass-through · uncertified';
  return fidelity?.mode === 'transformed' ? 'Transformed' : 'Strict';
}
