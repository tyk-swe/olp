import {
  limitForm,
  limitsInput,
  limitsError,
  type LimitForm
} from '../budgets/limitForm';
import type { components } from '$lib/api/schema';

export type EndUserPolicy = components['schemas']['EndUserPolicy'];
export type PolicyForm = {
  enabled: boolean;
  limitTemplate: string;
  defaults: LimitForm;
  overrides: { id: string; digest: string; limits: LimitForm }[];
  blocked: string;
};

export function policyForm(policy?: EndUserPolicy | null): PolicyForm {
  return {
    enabled: Boolean(policy),
    limitTemplate: policy?.limit_template ?? '',
    defaults: limitForm(policy?.defaults),
    overrides: Object.entries(policy?.overrides ?? {}).map(
      ([digest, limits]) => ({
        id: crypto.randomUUID(),
        digest,
        limits: limitForm(limits)
      })
    ),
    blocked: policy?.blocked?.join('\n') ?? ''
  };
}

export function policyError(form: PolicyForm): string {
  if (!form.enabled) return '';
  if (
    form.limitTemplate &&
    !/^[a-z0-9][a-z0-9._-]{0,99}$/.test(form.limitTemplate.trim())
  )
    return 'Enter a valid template name.';
  const digests = form.overrides.map(({ digest }) => digest.trim());
  const blocked = form.blocked.split(/\s+/).filter(Boolean);
  if (digests.length > 256 || blocked.length > 256)
    return 'Use at most 256 overrides and 256 blocked digests.';
  if ([...digests, ...blocked].some((digest) => !/^[0-9a-f]{64}$/.test(digest)))
    return 'Enter lowercase SHA-256 digests from the end-user lookup.';
  if (
    new Set(digests).size !== digests.length ||
    new Set(blocked).size !== blocked.length
  )
    return 'Each digest can appear only once in each list.';
  for (const limits of [
    form.defaults,
    ...form.overrides.map(({ limits }) => limits)
  ]) {
    const error = limitsError(limits);
    if (error) return error;
  }
  return '';
}

export function policyInput(form: PolicyForm): EndUserPolicy | null {
  if (!form.enabled) return null;
  return {
    ...(form.limitTemplate.trim()
      ? { limit_template: form.limitTemplate.trim() }
      : {}),
    defaults: limitsInput(form.defaults),
    overrides: Object.fromEntries(
      form.overrides.map(({ digest, limits }) => [
        digest.trim(),
        limitsInput(limits)
      ])
    ),
    blocked: form.blocked.split(/\s+/).filter(Boolean)
  };
}
