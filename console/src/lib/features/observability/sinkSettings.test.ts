import { describe, expect, it } from 'vitest';
import {
  buildSinkSettings,
  validateSinkSettings,
  type SinkSettingsDraft
} from './sinkSettings';

const draft: SinkSettingsDraft = {
  format: 'stripe',
  focusCostBasis: 'cost',
  focusStartDate: '',
  ceSource: '',
  stripeEvent: 'olp.charge',
  stripeValue: 'charge',
  customers: [{ project: 'project-one', customer: 'cus_123' }],
  stripeCustomerKey: '',
  stripeValueKey: ''
};
const credential = { editing: false, stripeKey: 'rk_test_key' };

describe('buildSinkSettings', () => {
  it('leaves settings absent for ordinary formats and a blank CloudEvents source', () => {
    for (const format of ['json', 'jsonl', 'otlp', 'cloudevents']) {
      expect(
        buildSinkSettings({ ...draft, format, ceSource: '  ' })
      ).toBeNull();
    }
    expect(
      buildSinkSettings({ ...draft, format: 'cloudevents', ceSource: ' /olp ' })
    ).toEqual({ source: '/olp' });
  });

  it('omits an empty FOCUS start date and retains the chosen cost basis', () => {
    expect(buildSinkSettings({ ...draft, format: 'csv_gzip' })).toEqual({
      cost_basis: 'cost'
    });
    expect(
      buildSinkSettings({
        ...draft,
        format: 'csv_gzip',
        focusCostBasis: 'charge',
        focusStartDate: '2026-10-01'
      })
    ).toEqual({ cost_basis: 'charge', start_date: '2026-10-01' });
  });

  it('trims Stripe values and preserves the last mapping for a duplicate project', () => {
    expect(
      buildSinkSettings({
        ...draft,
        stripeEvent: ' olp.charge ',
        stripeCustomerKey: ' customer ',
        stripeValueKey: ' amount ',
        customers: [
          ...draft.customers,
          { project: '', customer: 'cus_ignored' },
          { project: 'ignored', customer: '  ' },
          { project: 'project-one', customer: ' cus_latest ' }
        ]
      })
    ).toEqual({
      event_name: 'olp.charge',
      value: 'charge',
      customers: { 'project-one': 'cus_latest' },
      customer_key: 'customer',
      value_key: 'amount'
    });
    expect(buildSinkSettings(draft)).not.toHaveProperty('customer_key');
    expect(buildSinkSettings(draft)).not.toHaveProperty('value_key');
  });
});

describe('validateSinkSettings', () => {
  it('preserves the rolling FOCUS cutoff, including time of day', () => {
    const now = Date.parse('2026-10-14T00:00:00Z');
    const focus = {
      ...draft,
      format: 'csv_gzip',
      focusStartDate: '2026-07-17'
    };
    expect(validateSinkSettings(focus, credential, now)).toBe('');
    for (const date of ['2026-07-16', 'invalid']) {
      expect(
        validateSinkSettings(
          { ...focus, focusStartDate: date },
          credential,
          now
        )
      ).toBe('Start date must be within the last 90 days.');
    }
    expect(validateSinkSettings(focus, credential, now + 1)).toBe(
      'Start date must be within the last 90 days.'
    );
    // The form's max attribute handles future dates; the helper only checks age.
    for (const date of ['', '2026-10-15']) {
      expect(
        validateSinkSettings(
          { ...focus, focusStartDate: date },
          credential,
          now
        )
      ).toBe('');
    }
  });

  it('requires a key only for creation and reports it before other errors', () => {
    expect(
      validateSinkSettings(
        { ...draft, stripeEvent: '', customers: [] },
        { editing: false, stripeKey: '  ' }
      )
    ).toBe('An enabled Stripe sink needs a restricted API key.');
    expect(validateSinkSettings(draft, { editing: true, stripeKey: '' })).toBe(
      ''
    );
  });

  it.each([
    ['a', ''],
    ['a'.repeat(100), ''],
    [' A0_.:- ', ''],
    [
      '',
      'The Stripe event name must be 1-100 characters of letters, digits, _, ., : or -.'
    ],
    [
      'a'.repeat(101),
      'The Stripe event name must be 1-100 characters of letters, digits, _, ., : or -.'
    ],
    [
      'é',
      'The Stripe event name must be 1-100 characters of letters, digits, _, ., : or -.'
    ]
  ])('validates the Stripe event name %j', (stripeEvent, message) => {
    expect(validateSinkSettings({ ...draft, stripeEvent }, credential)).toBe(
      message
    );
  });

  it.each([
    [[], 'Map at least one project to a Stripe customer.'],
    [
      [{ project: '', customer: '  ' }],
      'Map at least one project to a Stripe customer.'
    ],
    [
      [{ project: '', customer: 'cus_1' }],
      'Each customer mapping needs a project.'
    ],
    [
      [{ project: 'one', customer: '' }],
      'Stripe customer IDs look like cus_….'
    ],
    [
      [{ project: 'one', customer: 'cus_' }],
      'Stripe customer IDs look like cus_….'
    ],
    [
      [{ project: 'one', customer: 'cus_' + 'a'.repeat(251) }],
      'Stripe customer IDs look like cus_….'
    ],
    [
      [{ project: 'one', customer: 'cus_a-b' }],
      'Stripe customer IDs look like cus_….'
    ],
    [[{ project: 'one', customer: ' cus_' + 'a'.repeat(250) + ' ' }], '']
  ])('validates customer mappings %#', (customers, message) => {
    expect(validateSinkSettings({ ...draft, customers }, credential)).toBe(
      message
    );
  });

  it.each(['stripeCustomerKey', 'stripeValueKey'] as const)(
    'validates the optional %s boundary',
    (field) => {
      for (const value of ['', '  ', ' A0_ ', 'a'.repeat(64)]) {
        expect(
          validateSinkSettings({ ...draft, [field]: value }, credential)
        ).toBe('');
      }
      for (const value of ['a'.repeat(65), 'a-b', 'é']) {
        expect(
          validateSinkSettings({ ...draft, [field]: value }, credential)
        ).toBe(
          `The ${field === 'stripeCustomerKey' ? 'customer' : 'value'} key must be 1-64 letters, digits or underscores.`
        );
      }
    }
  );
});
