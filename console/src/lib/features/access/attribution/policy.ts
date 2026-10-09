import type { components } from '$lib/api/schema';
export type AttributionPolicy = components['schemas']['AttributionPolicy'];
export type AttributionForm = {
  required: string;
  defaults: { id: string; key: string; value: string }[];
};
const keyPattern = /^[A-Za-z][A-Za-z0-9_.-]{0,31}$/;
const valuePattern = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$/;
export function attributionForm(
  policy?: AttributionPolicy | null
): AttributionForm {
  return {
    required: policy?.required_attribution_keys?.join(', ') ?? '',
    defaults: Object.entries(policy?.attribution_defaults ?? {}).map(
      ([key, value]) => ({ id: crypto.randomUUID(), key, value })
    )
  };
}
function requiredKeys(form: AttributionForm) {
  return form.required.split(/[,\s]+/).filter(Boolean);
}
export function attributionError(form: AttributionForm): string {
  const required = requiredKeys(form);
  const pins = form.defaults.map(({ key }) => key);
  if (new Set([...required, ...pins]).size > 4)
    return 'Use at most four distinct required or pinned attribution keys.';
  if (
    new Set(required).size !== required.length ||
    new Set(pins).size !== pins.length
  )
    return 'Attribution requirements and defaults must each have unique keys.';
  if ([...required, ...pins].some((key) => !keyPattern.test(key)))
    return 'Attribution keys must start with a letter and contain at most 32 letters, digits, dots, underscores or hyphens.';
  if (form.defaults.some(({ value }) => !valuePattern.test(value)))
    return 'Pinned values must be machine tokens of 1–64 characters.';
  return '';
}
export function attributionInput(
  form: AttributionForm
): AttributionPolicy | null {
  const required = requiredKeys(form);
  if (!required.length && !form.defaults.length) return null;
  return {
    required_attribution_keys: required,
    attribution_defaults: Object.fromEntries(
      form.defaults.map(({ key, value }) => [key, value])
    )
  };
}
