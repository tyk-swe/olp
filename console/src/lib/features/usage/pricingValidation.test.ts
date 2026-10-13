import { expect, it } from 'vitest';
import { optionalDecimal } from './pricingValidation';

it('keeps exact decimal strings and missing prices distinct from zero', () => {
  expect(optionalDecimal(' 0.00012500 ')).toBe('0.00012500');
  expect(optionalDecimal('9007199254740993.000000001')).toBe(
    '9007199254740993.000000001'
  );
  expect(optionalDecimal('0')).toBe('0');
  expect(optionalDecimal('')).toBeNull();
  expect(optionalDecimal(' ')).toBeNull();
});

it.each(['-1', '1e2', 'NaN', 'Infinity', '1,000', '.5', '1.'])(
  'rejects invalid decimal %s',
  (value) => {
    expect(() => optionalDecimal(value)).toThrow('non-negative decimal');
  }
);

import { tierRows, toolFeeRows, windowsDraft } from './pricingValidation';

it('turns tool fee rows into a tool_calls map, skipping blank rows', () => {
  expect(
    toolFeeRows([
      { name: 'web_search', fee: '0.010' },
      { name: '', fee: '' },
      { name: 'file_search', fee: '0' }
    ])
  ).toEqual({ web_search: '0.010', file_search: '0' });
  expect(toolFeeRows([])).toBeNull();
});

it.each([
  [
    [{ name: 'Web Search', fee: '1' }],
    'lowercase letters, digits and underscores'
  ],
  [[{ name: 'tool', fee: '' }], 'Enter a fee for tool tool'],
  [[{ name: '', fee: '1' }], 'Enter a tool name'],
  [
    [
      { name: 'tool', fee: '1' },
      { name: 'tool', fee: '2' }
    ],
    'listed twice'
  ]
])('rejects invalid tool rows %j', (rows, message) => {
  expect(() => toolFeeRows(rows)).toThrow(message);
});

it('builds tiers over exactly the fields the entry fills', () => {
  const tiers = tierRows(
    [
      {
        threshold: '1000',
        rates: { input_per_million: '3.00', output_per_million: '8.00' }
      },
      {
        threshold: '10000',
        rates: { input_per_million: '2.00', output_per_million: '6.00' }
      }
    ],
    ['input_per_million', 'output_per_million']
  );
  expect(tiers).toEqual([
    {
      above_input_tokens: 1000,
      input_per_million: '3.00',
      output_per_million: '8.00'
    },
    {
      above_input_tokens: 10000,
      input_per_million: '2.00',
      output_per_million: '6.00'
    }
  ]);
  expect(tierRows([], ['input_per_million'])).toBeNull();
  expect(() =>
    tierRows(
      [
        { threshold: '10', rates: { input_per_million: '1' } },
        { threshold: '10', rates: { input_per_million: '1' } }
      ],
      ['input_per_million']
    )
  ).toThrow('strictly increasing');
  expect(() =>
    tierRows([{ threshold: '10', rates: {} }], ['input_per_million'])
  ).toThrow('input per million rate');
});

it('builds windows only when a zone and periods are filled', () => {
  expect(windowsDraft('', [])).toBeNull();
  expect(
    windowsDraft('America/New_York', [
      { days: [1, 3], start: '09:00', end: '24:00', multiplier: '0.75' },
      { days: [], start: '', end: '', multiplier: '' }
    ])
  ).toEqual({
    time_zone: 'America/New_York',
    periods: [
      { days: [1, 3], start: '09:00', end: '24:00', multiplier: '0.75' }
    ]
  });
  expect(() =>
    windowsDraft('Not/AZone', [
      { days: [1], start: '00:00', end: '01:00', multiplier: '1' }
    ])
  ).toThrow('Unknown IANA time zone');
  expect(() =>
    windowsDraft('UTC', [
      { days: [], start: '00:00', end: '01:00', multiplier: '1' }
    ])
  ).toThrow('at least one day');
  expect(() =>
    windowsDraft('UTC', [
      { days: [1], start: '10:00', end: '09:00', multiplier: '1' }
    ])
  ).toThrow('ends after it starts');
  expect(() =>
    windowsDraft('UTC', [
      { days: [1], start: '10:00', end: '11:00', multiplier: '' }
    ])
  ).toThrow('needs a multiplier');
});
