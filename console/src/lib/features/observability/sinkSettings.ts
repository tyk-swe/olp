export type SinkSettingsDraft = {
  format: string;
  focusCostBasis: 'cost' | 'charge';
  focusStartDate: string;
  ceSource: string;
  stripeEvent: string;
  stripeValue: string;
  customers: { project: string; customer: string }[];
  stripeCustomerKey: string;
  stripeValueKey: string;
};

const stripeEventPattern = /^[A-Za-z0-9_.:-]{1,100}$/;
const stripeCustomerPattern = /^cus_[A-Za-z0-9]{1,250}$/;
const stripeFieldPattern = /^[A-Za-z0-9_]{1,64}$/;

// buildSinkSettings builds the format-specific settings document. The caller
// sends null on creation and omits the field on edits when there are no settings.
export function buildSinkSettings(
  draft: SinkSettingsDraft
): Record<string, unknown> | null {
  if (draft.format === 'csv_gzip') {
    const settings: Record<string, unknown> = {
      cost_basis: draft.focusCostBasis
    };
    if (draft.focusStartDate) settings.start_date = draft.focusStartDate;
    return settings;
  }
  if (draft.format === 'cloudevents') {
    return draft.ceSource.trim() ? { source: draft.ceSource.trim() } : null;
  }
  if (draft.format === 'stripe') {
    const settings: Record<string, unknown> = {
      event_name: draft.stripeEvent.trim(),
      value: draft.stripeValue,
      customers: Object.fromEntries(
        draft.customers
          .filter((row) => row.project && row.customer.trim())
          .map((row) => [row.project, row.customer.trim()])
      )
    };
    if (draft.stripeCustomerKey.trim())
      settings.customer_key = draft.stripeCustomerKey.trim();
    if (draft.stripeValueKey.trim())
      settings.value_key = draft.stripeValueKey.trim();
    return settings;
  }
  return null;
}

// validateSinkSettings mirrors the server's settings rules so a bad form
// stops at the browser rather than the API.
export function validateSinkSettings(
  draft: SinkSettingsDraft,
  credential: { editing: boolean; stripeKey: string },
  now = Date.now()
): string {
  if (draft.format === 'csv_gzip' && draft.focusStartDate) {
    const start = Date.parse(draft.focusStartDate + 'T00:00:00Z');
    const earliest = now - 89 * 24 * 60 * 60 * 1000;
    if (!Number.isFinite(start) || start < earliest)
      return 'Start date must be within the last 90 days.';
  }
  if (draft.format === 'stripe') {
    if (!credential.editing && !credential.stripeKey.trim())
      return 'An enabled Stripe sink needs a restricted API key.';
    if (!stripeEventPattern.test(draft.stripeEvent.trim()))
      return 'The Stripe event name must be 1-100 characters of letters, digits, _, ., : or -.';
    const rows = draft.customers.filter(
      (row) => row.project || row.customer.trim()
    );
    if (!rows.length) return 'Map at least one project to a Stripe customer.';
    for (const row of rows) {
      if (!row.project) return 'Each customer mapping needs a project.';
      if (!stripeCustomerPattern.test(row.customer.trim()))
        return 'Stripe customer IDs look like cus_….';
    }
    if (
      draft.stripeCustomerKey.trim() &&
      !stripeFieldPattern.test(draft.stripeCustomerKey.trim())
    )
      return 'The customer key must be 1-64 letters, digits or underscores.';
    if (
      draft.stripeValueKey.trim() &&
      !stripeFieldPattern.test(draft.stripeValueKey.trim())
    )
      return 'The value key must be 1-64 letters, digits or underscores.';
  }
  return '';
}
