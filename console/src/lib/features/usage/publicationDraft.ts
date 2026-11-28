import { captureResult } from '#lib/editing/result.js';
import { validated, type Validation } from '#lib/editing/validation.js';
import type { PriceDraft } from './pricing';
import type { PublishPricingSourceInput } from './pricingSources';

export type PublicationField = 'effective_at' | 'overrides';

export const publicationServerFields: Readonly<
  Record<string, PublicationField>
> = { effective_at: 'effective_at', overrides: 'overrides' };

export function validatePublication(
  effectiveAt: string,
  overrides: string
): Validation<PublishPricingSourceInput, PublicationField> {
  const parsed = overrides.trim()
    ? captureResult((): unknown => JSON.parse(overrides))
    : null;
  const entries =
    parsed?.ok && Array.isArray(parsed.value)
      ? (parsed.value as PriceDraft[])
      : undefined;
  return validated(
    {
      overrides:
        parsed && !entries
          ? 'Overrides must be a JSON array of price entries.'
          : undefined
    },
    () => ({
      effective_at: effectiveAt ? new Date(effectiveAt).toISOString() : null,
      overrides: entries
    })
  );
}
