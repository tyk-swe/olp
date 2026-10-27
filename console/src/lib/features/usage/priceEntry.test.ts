import { describe, expect, it } from 'vitest';
import {
  containerFeeRows,
  emptyPriceFields,
  priceEntryFields
} from './priceEntry';

describe('retained price fields', () => {
  it('preserves exact decimal strings and native container memory tiers', () => {
    const values = emptyPriceFields();
    values.trainingPrice = ' 8.123456789012 ';
    values.storagePrice = '0.100000000001';
    values.cacheStoragePrice = '2.000000000003';
    values.containerFees = [
      { tier: '1g', fee: '0.030000000001' },
      { tier: '4g', fee: '0' }
    ];
    const result = priceEntryFields(values);
    expect(result).toMatchObject({
      training_per_million: '8.123456789012',
      storage_per_gib_day: '0.100000000001',
      cache_storage_per_million_token_hour: '2.000000000003',
      container_per_session: { '1g': '0.030000000001', '4g': '0' }
    });
    expect(result.per_request).toBeNull();
    expect(result.tiers).toBeNull();
  });
  it('distinguishes absent storage prices from explicit zero rates', () => {
    const values = emptyPriceFields();
    expect(priceEntryFields(values).storage_per_gib_day).toBeNull();
    values.storagePrice = '0';
    expect(priceEntryFields(values).storage_per_gib_day).toBe('0');
    expect(containerFeeRows([{ tier: '', fee: '' }])).toBeNull();
  });
  it('accepts tool names that also name inherited object properties', () => {
    const result = priceEntryFields({
      ...emptyPriceFields(),
      toolFees: [
        { name: ' constructor ', fee: ' 0.125000000001 ' },
        { name: 'search', fee: '0' }
      ]
    });
    expect(result.tool_calls).toEqual({
      constructor: '0.125000000001',
      search: '0'
    });
  });
  it('refuses duplicate tool fees after trimming their names', () => {
    for (const name of ['constructor', 'search']) {
      expect(() =>
        priceEntryFields({
          ...emptyPriceFields(),
          toolFees: [
            { name, fee: '1' },
            { name: ` ${name} `, fee: '2' }
          ]
        })
      ).toThrow(`Tool ${name} is listed twice.`);
    }
  });
  it('refuses malformed, duplicate or unpriced memory tiers', () => {
    for (const rows of [
      [
        { tier: '1g', fee: '0.03' },
        { tier: '1g', fee: '0.06' }
      ],
      [{ tier: '0g', fee: '0.03' }],
      [{ tier: '4GB', fee: '0.03' }],
      [{ tier: '', fee: '0.03' }],
      [{ tier: '1g', fee: '' }],
      [{ tier: '1g', fee: '-1' }]
    ])
      expect(() => containerFeeRows(rows)).toThrow();
  });
});
