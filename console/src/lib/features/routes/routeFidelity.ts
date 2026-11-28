import type { components } from '#lib/management/schema.js';

export type FidelityMode = components['schemas']['RouteFidelity']['mode'];

/** Route fidelity choices. Routes are strict unless declared transformed. */
export const fidelityOptions: ReadonlyArray<{
  value: FidelityMode;
  label: string;
}> = [
  { value: 'strict', label: 'Strict · preserve the native invocation' },
  {
    value: 'transformed',
    label: 'Transformed · translate or redact deliberately'
  }
];

export function fidelityLabel(
  fidelity?: components['schemas']['RouteFidelity'] | null,
  passthrough?: components['schemas']['PassthroughConfig'] | null
): 'Strict' | 'Transformed' | 'Pass-through · uncertified' {
  if (passthrough) return 'Pass-through · uncertified';
  return fidelity?.mode === 'transformed' ? 'Transformed' : 'Strict';
}
