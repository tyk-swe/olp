import { describe, expect, it } from 'vitest';
import {
  containerFeeRows,
  emptyPriceFields,
  priceEntryFields,
  type WindowPeriodRow
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
  it('drops nameless tool fee rows but still wants the fee filled', () => {
    const values = emptyPriceFields();
    values.toolFees = [
      { name: '', fee: '' },
      { name: '', fee: ' ' },
      { name: 'search', fee: '0.5' }
    ];
    expect(priceEntryFields(values).tool_calls).toEqual({ search: '0.5' });
    expect(
      priceEntryFields({ ...emptyPriceFields(), toolFees: [] }).tool_calls
    ).toBeNull();
    values.toolFees = [
      { name: ' ', fee: '1' },
      { name: 'search', fee: '0.5' }
    ];
    expect(() => priceEntryFields(values)).toThrow(
      'Enter a tool name for every tool fee.'
    );
  });
  it('refuses unpriced, malformed and overflowing tool fees', () => {
    const fields = (toolFees: { name: string; fee: string }[]) => ({
      ...emptyPriceFields(),
      toolFees
    });
    expect(() =>
      priceEntryFields(fields([{ name: 'search', fee: ' ' }]))
    ).toThrow('Enter a fee for tool search.');
    for (const name of ['Search', '2fa', 'web-search', 'a'.repeat(65)])
      expect(() => priceEntryFields(fields([{ name, fee: '1' }]))).toThrow(
        'Tool names are lowercase letters, digits and underscores'
      );
    expect(() =>
      priceEntryFields(fields([{ name: 'search', fee: '1.2.3' }]))
    ).toThrow('Enter a non-negative decimal number.');
    expect(() =>
      priceEntryFields(
        fields(
          Array.from({ length: 17 }, (_, i) => ({
            name: `tool_${i}`,
            fee: '1'
          }))
        )
      )
    ).toThrow('At most 16 tool fees per price.');
    expect(
      priceEntryFields(
        fields(
          Array.from({ length: 16 }, (_, i) => ({
            name: `tool_${i}`,
            fee: '1'
          }))
        )
      ).tool_calls
    ).toHaveProperty('tool_15', '1');
  });
  it('maps increasing tiers with the filled rate fields', () => {
    const values = emptyPriceFields();
    values.inputPrice = '1';
    values.outputPrice = '2.5';
    values.tiers = [
      {
        threshold: ' 1000 ',
        rates: { input_per_million: '0.5', output_per_million: '1' }
      },
      {
        threshold: '2000',
        rates: { input_per_million: '0.25', output_per_million: '0.75' }
      }
    ];
    expect(priceEntryFields(values).tiers).toEqual([
      {
        above_input_tokens: 1000,
        input_per_million: '0.5',
        output_per_million: '1'
      },
      {
        above_input_tokens: 2000,
        input_per_million: '0.25',
        output_per_million: '0.75'
      }
    ]);
    expect(
      priceEntryFields({ ...emptyPriceFields(), tiers: [] }).tiers
    ).toBeNull();
  });
  it('refuses tiers without rates, positive order or within the limit', () => {
    const fields = (
      tiers: { threshold: string; rates: Record<string, string> }[]
    ) => {
      const values = emptyPriceFields();
      values.inputPrice = '1';
      values.tiers = tiers;
      return values;
    };
    expect(() =>
      priceEntryFields({
        ...emptyPriceFields(),
        tiers: [{ threshold: '1000', rates: {} }]
      })
    ).toThrow('Tiers replace the per-million rates; enter a rate first.');
    for (const threshold of ['0', 'abc', '1.5', '-5']) {
      const values = fields([{ threshold, rates: { input_per_million: '1' } }]);
      expect(() => priceEntryFields(values)).toThrow(
        'Tier 1 needs a positive token threshold.'
      );
    }
    for (const thresholds of [
      ['1000', '1000'],
      ['2000', '1000']
    ]) {
      const values = fields(
        thresholds.map((threshold) => ({
          threshold,
          rates: { input_per_million: '1' }
        }))
      );
      expect(() => priceEntryFields(values)).toThrow(
        'Tier thresholds must be strictly increasing.'
      );
    }
    expect(() =>
      priceEntryFields(
        fields([{ threshold: '1000', rates: { output_per_million: '1' } }])
      )
    ).toThrow('Tier 1 needs a input per million rate.');
    expect(
      priceEntryFields(
        fields(
          Array.from({ length: 8 }, (_, i) => ({
            threshold: `${(i + 1) * 1000}`,
            rates: { input_per_million: '1' }
          }))
        )
      ).tiers
    ).toHaveLength(8);
    expect(() =>
      priceEntryFields(
        fields(
          Array.from({ length: 9 }, (_, i) => ({
            threshold: `${(i + 1) * 1000}`,
            rates: { input_per_million: '1' }
          }))
        )
      )
    ).toThrow('At most 8 tiers per price.');
  });
  it('maps window periods with sorted days and the zone', () => {
    const values = emptyPriceFields();
    values.windowZone = ' Europe/Berlin ';
    values.windowPeriods = [
      { days: [], start: '', end: '', multiplier: '' },
      { days: [6, 0], start: '20:00', end: '24:00', multiplier: '0.5' }
    ];
    expect(priceEntryFields(values).windows).toEqual({
      time_zone: 'Europe/Berlin',
      periods: [
        { days: [0, 6], start: '20:00', end: '24:00', multiplier: '0.5' }
      ]
    });
    expect(
      priceEntryFields({
        ...emptyPriceFields(),
        windowPeriods: [{ days: [], start: '', end: '', multiplier: '' }]
      }).windows
    ).toBeNull();
  });
  it('refuses windows without a zone, periods or well-formed times', () => {
    const fields = (
      windowZone: string,
      windowPeriods: {
        days: number[];
        start: string;
        end: string;
        multiplier: string;
      }[]
    ) => ({ ...emptyPriceFields(), windowZone, windowPeriods });
    expect(() =>
      priceEntryFields(
        fields(' ', [
          { days: [1], start: '08:00', end: '09:00', multiplier: '1' }
        ])
      )
    ).toThrow('Enter an IANA time zone for the windows.');
    expect(() =>
      priceEntryFields(
        fields('Mars/Olympus', [
          { days: [1], start: '08:00', end: '09:00', multiplier: '1' }
        ])
      )
    ).toThrow('Unknown IANA time zone Mars/Olympus.');
    expect(() => priceEntryFields(fields('UTC', []))).toThrow(
      'Add at least one window period.'
    );
    expect(() =>
      priceEntryFields(
        fields(
          'UTC',
          Array.from({ length: 33 }, () => ({
            days: [1],
            start: '08:00',
            end: '09:00',
            multiplier: '1'
          }))
        )
      )
    ).toThrow('At most 32 window periods.');
    expect(() =>
      priceEntryFields(
        fields('UTC', [
          { days: [], start: '08:00', end: '09:00', multiplier: '1' }
        ])
      )
    ).toThrow('Window 1 needs at least one day.');
    expect(
      priceEntryFields(
        fields(
          'UTC',
          Array.from({ length: 32 }, () => ({
            days: [1],
            start: '08:00',
            end: '09:00',
            multiplier: '1'
          }))
        )
      ).windows?.periods
    ).toHaveLength(32);
    expect(
      priceEntryFields({ ...emptyPriceFields(), windowZone: ' ' }).windows
    ).toBeNull();
  });
  it('keeps window periods with any filled field and drops blank ones', () => {
    const fields = (windowPeriods: WindowPeriodRow[]) => ({
      ...emptyPriceFields(),
      windowZone: 'UTC',
      windowPeriods
    });
    expect(() =>
      priceEntryFields(
        fields([{ days: [1], start: '', end: '', multiplier: '' }])
      )
    ).toThrow('Window 1 start is a HH:MM time.');
    expect(() =>
      priceEntryFields(
        fields([{ days: [], start: '08:00', end: '', multiplier: '' }])
      )
    ).toThrow('Window 1 needs at least one day.');
    for (const windowPeriods of [
      [{ days: [], start: ' ', end: '', multiplier: '' }],
      [{ days: [], start: '', end: ' ', multiplier: '' }],
      [{ days: [], start: '', end: '', multiplier: ' ' }]
    ])
      expect(() => priceEntryFields(fields(windowPeriods))).toThrow(
        'Add at least one window period.'
      );
  });
  it('refuses windows with inverted, open-ended or unpriced times', () => {
    const period = (
      over: Partial<{
        days: number[];
        start: string;
        end: string;
        multiplier: string;
      }>
    ) => ({
      ...emptyPriceFields(),
      windowZone: 'UTC',
      windowPeriods: [
        { days: [1], start: '08:00', end: '09:00', multiplier: '1', ...over }
      ]
    });
    expect(() => priceEntryFields(period({ start: '9:00' }))).toThrow(
      'Window 1 start is a HH:MM time.'
    );
    expect(() => priceEntryFields(period({ start: '108:00' }))).toThrow(
      'Window 1 start is a HH:MM time.'
    );
    expect(() => priceEntryFields(period({ end: '08:000' }))).toThrow(
      'Window 1 end is a HH:MM time.'
    );
    expect(() => priceEntryFields(period({ start: '24:00' }))).toThrow(
      'Window 1 starts before 24:00.'
    );
    expect(() => priceEntryFields(period({ end: '25:00' }))).toThrow(
      'Window 1 end is a HH:MM time.'
    );
    expect(() => priceEntryFields(period({ end: '08:00' }))).toThrow(
      'Window 1 ends after it starts.'
    );
    expect(() => priceEntryFields(period({ end: '07:59' }))).toThrow(
      'Window 1 ends after it starts.'
    );
    expect(() => priceEntryFields(period({ multiplier: ' ' }))).toThrow(
      'Window 1 needs a multiplier.'
    );
    expect(() => priceEntryFields(period({ multiplier: 'abc' }))).toThrow(
      'Enter a non-negative decimal number.'
    );
  });
  it('trims window times and keeps multi-digit decimals intact', () => {
    const values = emptyPriceFields();
    values.inputPrice = '12.50';
    values.windowZone = 'UTC';
    values.windowPeriods = [
      { days: [2], start: ' 08:00 ', end: ' 24:00 ', multiplier: '1' }
    ];
    const result = priceEntryFields(values);
    expect(result.input_per_million).toBe('12.50');
    expect(result.windows).toMatchObject({
      periods: [{ days: [2], start: '08:00', end: '24:00', multiplier: '1' }]
    });
  });
});
