import { describe, expect, it } from 'vitest';
import {
  parseSinkSettings,
  parseSinkCredential,
  newSinkDelivery,
  changeSinkDelivery,
  toggleSinkStream,
  type SinkSettingsDraft
} from './sinkSettings';

const stripe: Extract<SinkSettingsDraft, { format: 'stripe' }> = {
  format: 'stripe',
  stripeEvent: 'usage-recorded',
  stripeValue: 'cost',
  customers: [{ project: 'project-a', customer: 'cus_ABC123' }],
  stripeCustomerKey: '',
  stripeValueKey: ''
};

function settings(draft: SinkSettingsDraft) {
  const parsed = parseSinkSettings(draft, { editing: true, stripeKey: '' });
  expect(parsed.kind).toBe('valid');
  return parsed.kind === 'valid' ? parsed.settings : null;
}
function issue(
  draft: SinkSettingsDraft,
  credential: { editing: boolean; stripeKey: string }
) {
  const parsed = parseSinkSettings(draft, credential);
  return parsed.kind === 'invalid' ? parsed.message : '';
}

const createCredential = { editing: false, stripeKey: 'restricted-key' };

describe('export sink settings', () => {
  it('builds cost or charge settings with an optional start date', () => {
    expect(
      settings({
        ...stripe,
        format: 'csv_gzip',
        focusCostBasis: 'charge',
        focusStartDate: ''
      })
    ).toEqual({ cost_basis: 'charge' });
    expect(
      settings({
        ...stripe,
        format: 'csv_gzip',
        focusCostBasis: 'cost',
        focusStartDate: '2025-01-02'
      })
    ).toEqual({ cost_basis: 'cost', start_date: '2025-01-02' });
  });

  it('includes only a nonblank, trimmed CloudEvents source', () => {
    expect(
      settings({
        ...stripe,
        format: 'cloudevents',
        ceSource: '  urn:source  '
      })
    ).toEqual({ source: 'urn:source' });
    expect(
      settings({ ...stripe, format: 'cloudevents', ceSource: '  ' })
    ).toBeNull();
    expect(settings({ ...stripe, format: 'json' })).toBeNull();
  });

  it('builds Stripe settings from complete mappings and optional field names', () => {
    expect(
      settings({
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
    expect(settings(stripe)).not.toHaveProperty('customer_key');
    expect(settings(stripe)).not.toHaveProperty('value_key');
  });

  it('requires a restricted key for creation but permits edits to keep their credential', () => {
    expect(issue(stripe, { editing: false, stripeKey: ' ' })).toContain(
      'restricted API key'
    );
    expect(issue(stripe, { editing: true, stripeKey: '' })).toBe('');
    expect(
      issue(
        { format: 'csv_gzip', focusCostBasis: 'cost', focusStartDate: '' },
        { editing: false, stripeKey: '' }
      )
    ).toBe('');
  });

  it.each(['', 'invalid event', 'x'.repeat(101)])(
    'rejects an invalid event name %s',
    (stripeEvent) => {
      expect(issue({ ...stripe, stripeEvent }, createCredential)).toContain(
        'event name'
      );
    }
  );

  it.each([
    [[], 'Map at least one project'],
    [[{ project: '', customer: 'cus_ABC' }], 'needs a project'],
    [[{ project: 'project-a', customer: '' }], 'customer IDs'],
    [[{ project: 'project-a', customer: 'bad' }], 'customer IDs']
  ])('rejects incomplete or invalid customer mappings', (customers, error) => {
    expect(issue({ ...stripe, customers }, createCredential)).toContain(error);
  });

  it.each([
    ['stripeCustomerKey', 'customer key'],
    ['stripeValueKey', 'value key']
  ] as const)(
    'rejects an invalid %s while allowing a valid one',
    (field, error) => {
      expect(
        issue({ ...stripe, [field]: 'bad-key' }, createCredential)
      ).toContain(error);
      expect(
        issue({ ...stripe, [field]: ' valid_key_1 ' }, createCredential)
      ).toBe('');
    }
  );
});

it('changes delivery formats without carrying unsupported streams', () => {
  const audit = toggleSinkStream(newSinkDelivery('https'), 'audit');
  const objectStorage = changeSinkDelivery(audit, 's3', 'jsonl');
  expect(objectStorage.streams).toEqual(['requests', 'audit']);
  const focus = changeSinkDelivery(objectStorage, 's3', 'csv_gzip');
  expect(focus.streams).toEqual(['focus']);
  expect(toggleSinkStream(focus, 'audit')).toEqual(focus);
  expect(changeSinkDelivery(focus, 'https', 'json').streams).toEqual([]);
});

it('distinguishes retaining, clearing and replacing a sink credential', () => {
  const input = {
    type: 'https' as const,
    clear: false,
    stripeKey: '',
    json: ''
  };
  expect(parseSinkCredential(input)).toEqual({
    kind: 'valid',
    change: { kind: 'keep' }
  });
  expect(parseSinkCredential({ ...input, clear: true })).toEqual({
    kind: 'valid',
    change: { kind: 'clear' }
  });
  expect(parseSinkCredential({ ...input, json: '{"token":"secret"}' })).toEqual(
    { kind: 'valid', change: { kind: 'replace', value: { token: 'secret' } } }
  );
  expect(parseSinkCredential({ ...input, json: '["secret"]' }).kind).toBe(
    'invalid'
  );
});
