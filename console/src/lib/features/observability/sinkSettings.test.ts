import { describe, expect, it } from 'vitest';
import {
  buildSinkSettings,
  validateSinkSettings,
  type SinkSettingsDraft
} from './sinkSettings';

const stripe: SinkSettingsDraft = {
  format: 'stripe',
  focusCostBasis: 'cost',
  focusStartDate: '',
  ceSource: '',
  stripeEvent: 'usage-recorded',
  stripeValue: 'cost',
  customers: [{ project: 'project-a', customer: 'cus_ABC123' }],
  stripeCustomerKey: '',
  stripeValueKey: ''
};

const createCredential = { editing: false, stripeKey: 'restricted-key' };

describe('export sink settings', () => {
  it('builds cost or charge settings with an optional start date', () => {
    expect(
      buildSinkSettings({
        ...stripe,
        format: 'csv_gzip',
        focusCostBasis: 'charge'
      })
    ).toEqual({ cost_basis: 'charge' });
    expect(
      buildSinkSettings({
        ...stripe,
        format: 'csv_gzip',
        focusStartDate: '2025-01-02'
      })
    ).toEqual({ cost_basis: 'cost', start_date: '2025-01-02' });
  });

  it('includes only a nonblank, trimmed CloudEvents source', () => {
    expect(
      buildSinkSettings({
        ...stripe,
        format: 'cloudevents',
        ceSource: '  urn:source  '
      })
    ).toEqual({ source: 'urn:source' });
    expect(
      buildSinkSettings({ ...stripe, format: 'cloudevents', ceSource: '  ' })
    ).toBeNull();
    expect(buildSinkSettings({ ...stripe, format: 'json' })).toBeNull();
  });

  it('builds Stripe settings from complete mappings and optional field names', () => {
    expect(
      buildSinkSettings({
        ...stripe,
        stripeEvent: ' usage.recorded ',
        customers: [
          ...stripe.customers,
          { project: 'project-b', customer: ' cus_XYZ ' },
          { project: '', customer: '' }
        ],
        stripeCustomerKey: ' customer_id ',
        stripeValueKey: ' total '
      })
    ).toEqual({
      event_name: 'usage.recorded',
      value: 'cost',
      customers: { 'project-a': 'cus_ABC123', 'project-b': 'cus_XYZ' },
      customer_key: 'customer_id',
      value_key: 'total'
    });
    expect(buildSinkSettings(stripe)).not.toHaveProperty('customer_key');
    expect(buildSinkSettings(stripe)).not.toHaveProperty('value_key');
  });

  it('requires a restricted key for creation but permits edits to keep their credential', () => {
    expect(
      validateSinkSettings(stripe, { editing: false, stripeKey: ' ' })
    ).toContain('restricted API key');
    expect(validateSinkSettings(stripe, { editing: true, stripeKey: '' })).toBe(
      ''
    );
    expect(
      validateSinkSettings(
        { ...stripe, format: 'csv_gzip' },
        { editing: false, stripeKey: '' }
      )
    ).toBe('');
  });

  it.each(['', 'invalid event', 'x'.repeat(101)])(
    'rejects an invalid event name %s',
    (stripeEvent) => {
      expect(
        validateSinkSettings({ ...stripe, stripeEvent }, createCredential)
      ).toContain('event name');
    }
  );

  it.each([
    [[], 'Map at least one project'],
    [[{ project: '', customer: 'cus_ABC' }], 'needs a project'],
    [[{ project: 'project-a', customer: '' }], 'customer IDs'],
    [[{ project: 'project-a', customer: 'bad' }], 'customer IDs']
  ])('rejects incomplete or invalid customer mappings', (customers, error) => {
    expect(
      validateSinkSettings({ ...stripe, customers }, createCredential)
    ).toContain(error);
  });

  it.each([
    ['stripeCustomerKey', 'customer key'],
    ['stripeValueKey', 'value key']
  ] as const)(
    'rejects an invalid %s while allowing a valid one',
    (field, error) => {
      expect(
        validateSinkSettings(
          { ...stripe, [field]: 'bad-key' },
          createCredential
        )
      ).toContain(error);
      expect(
        validateSinkSettings(
          { ...stripe, [field]: ' valid_key_1 ' },
          createCredential
        )
      ).toBe('');
    }
  );
});
