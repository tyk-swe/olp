import { describe, expect, it } from 'vitest';
import { validateApiKey } from './keyValidation';

describe('API key validation', () => {
  it('requires an intentional name and positive hard limits', () => {
    expect(validateApiKey({ name: ' ', requestsPerMinute: 0 })).toMatchObject({
      name: 'Enter a name.',
      requestsPerMinute: expect.any(String)
    });
  });

  it('accepts an unlimited key when limit fields are omitted', () => {
    expect(validateApiKey({ name: 'production SDK' })).toEqual({});
  });

  it('accepts exact positive cost budgets within the storage precision', () => {
    expect(
      validateApiKey({
        name: 'production SDK',
        dailyCostLimit: '000000000001.000000000001',
        monthlyCostLimit: '999999999999.999999999999'
      })
    ).toEqual({});
  });

  it('rejects zero, signed, malformed, and oversized cost budgets', () => {
    expect(
      validateApiKey({
        name: 'production SDK',
        dailyCostLimit: '0.000000000000',
        monthlyCostLimit: '1000000000000'
      })
    ).toMatchObject({
      dailyCostLimit: expect.any(String),
      monthlyCostLimit: expect.any(String)
    });
    expect(
      validateApiKey({ name: 'production SDK', dailyCostLimit: '+1.00' })
    ).toMatchObject({ dailyCostLimit: expect.any(String) });
    expect(
      validateApiKey({ name: 'production SDK', dailyCostLimit: '1.2.3' })
    ).toMatchObject({ dailyCostLimit: expect.any(String) });
    expect(
      validateApiKey({
        name: 'production SDK',
        dailyCostLimit: '1.0000000000001'
      })
    ).toMatchObject({ dailyCostLimit: expect.any(String) });
  });

  it('rejects an expiry that has already passed', () => {
    const now = new Date('2026-07-12T12:00:00Z');

    expect(
      validateApiKey({
        name: 'production SDK',
        // A `datetime-local` value is wall time: the suite runs in
        // America/New_York, where this is 11:00Z and so already behind `now`.
        expiresAt: '2026-07-12T07:00',
        now
      })
    ).toMatchObject({ expiresAt: 'Choose an expiry in the future.' });
    expect(
      validateApiKey({ name: 'production SDK', expiresAt: 'not-a-date', now })
    ).toMatchObject({ expiresAt: 'Enter a valid expiry date and time.' });
  });

  it('accepts a future expiry and an omitted expiry', () => {
    const now = new Date('2026-07-12T12:00:00Z');

    expect(
      validateApiKey({
        name: 'production SDK',
        expiresAt: '2027-01-01T00:00',
        now
      })
    ).toEqual({});
    expect(
      validateApiKey({ name: 'production SDK', expiresAt: '', now })
    ).toEqual({});
  });
});
