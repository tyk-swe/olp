import type { components } from '$lib/api/schema';

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
