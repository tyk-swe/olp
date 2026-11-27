import { describe, expect, it } from 'vitest';
import {
  optionalDecimal,
  tierRows,
  toolFeeRows,
  toolNameText,
  windowsDraft
} from './pricingValidation';

describe('optionalDecimal and toolNameText', () => {
  it('trims valid decimals and rejects negative or malformed values', () => {
    expect(optionalDecimal('   ')).toBeNull();
    expect(optionalDecimal(' 0.125 ')).toBe('0.125');
    expect(optionalDecimal('42')).toBe('42');
    expect(() => optionalDecimal('-1')).toThrow('non-negative decimal');
    expect(() => optionalDecimal('1e5')).toThrow('non-negative decimal');
    expect(() => optionalDecimal('.5')).toThrow('non-negative decimal');
  });

  it('enforces tool name characters and 64-character bound', () => {
    expect(toolNameText(' web_search_2 ')).toBe('web_search_2');
    expect(toolNameText('a' + 'b'.repeat(63))).toHaveLength(64);
    expect(() => toolNameText('1bad')).toThrow('Tool names');
    expect(() => toolNameText('BadName')).toThrow('Tool names');
    expect(() => toolNameText('a' + 'b'.repeat(64))).toThrow('Tool names');
  });
});

describe('toolFeeRows', () => {
  it('ignores blank spare rows and validates names, fees, duplicates, and row limit', () => {
    expect(toolFeeRows([{ name: ' ', fee: ' ' }])).toBeNull();
    expect(
      toolFeeRows([
        { name: '', fee: '' },
        { name: ' web_search ', fee: ' 0.01 ' }
      ])
    ).toEqual({ web_search: '0.01' });

    expect(() => toolFeeRows([{ name: '', fee: '0.5' }])).toThrow(
      'Enter a tool name'
    );
    expect(() => toolFeeRows([{ name: 'search', fee: '' }])).toThrow(
      'Enter a fee for tool search.'
    );
    expect(() =>
      toolFeeRows([
        { name: 'search', fee: '0.1' },
        { name: 'search', fee: '0.2' }
      ])
    ).toThrow('listed twice');

    const seventeen = Array.from({ length: 17 }, (_, i) => ({
      name: `tool_${i}`,
      fee: '1'
    }));
    expect(() => toolFeeRows(seventeen)).toThrow('At most 16 tool fees');
  });
});

describe('tierRows', () => {
  it('validates thresholds, ordering, entry field completeness, and tier limit', () => {
    expect(tierRows([], ['input_per_million'])).toBeNull();
    expect(() => tierRows([{ threshold: '100', rates: {} }], [])).toThrow(
      'enter a rate first'
    );

    const valid = tierRows(
      [
        {
          threshold: '200000',
          rates: { input_per_million: '2.5', output_per_million: '10' }
        },
        {
          threshold: '1000000',
          rates: { input_per_million: '5', output_per_million: '20' }
        }
      ],
      ['input_per_million', 'output_per_million']
    );
    expect(valid).toEqual([
      {
        above_input_tokens: 200000,
        input_per_million: '2.5',
        output_per_million: '10'
      },
      {
        above_input_tokens: 1000000,
        input_per_million: '5',
        output_per_million: '20'
      }
    ]);

    expect(() =>
      tierRows(
        [{ threshold: '0', rates: { input_per_million: '1' } }],
        ['input_per_million']
      )
    ).toThrow('positive token threshold');

    expect(() =>
      tierRows(
        [
          { threshold: '500', rates: { input_per_million: '1' } },
          { threshold: '500', rates: { input_per_million: '2' } }
        ],
        ['input_per_million']
      )
    ).toThrow('strictly increasing');

    expect(() =>
      tierRows(
        [{ threshold: '100', rates: { input_per_million: '1' } }],
        ['input_per_million', 'output_per_million']
      )
    ).toThrow('needs a output per million rate');

    const nine = Array.from({ length: 9 }, (_, i) => ({
      threshold: String((i + 1) * 100),
      rates: { input_per_million: '1' }
    }));
    expect(() => tierRows(nine, ['input_per_million'])).toThrow(
      'At most 8 tiers'
    );
  });
});

describe('windowsDraft', () => {
  it('accepts valid windows including 24:00 end time and sorts days ascending', () => {
    expect(windowsDraft('  ', [])).toBeNull();
    expect(
      windowsDraft('UTC', [
        { days: [], start: '', end: '', multiplier: '' },
        { days: [5, 1, 3], start: '09:00', end: '24:00', multiplier: '1.25' }
      ])
    ).toEqual({
      time_zone: 'UTC',
      periods: [
        { days: [1, 3, 5], start: '09:00', end: '24:00', multiplier: '1.25' }
      ]
    });
  });

  it('rejects missing or unknown time zone, invalid times, empty days, and over 32 periods', () => {
    expect(() =>
      windowsDraft('', [
        { days: [1], start: '00:00', end: '12:00', multiplier: '1' }
      ])
    ).toThrow('IANA time zone');

    expect(() =>
      windowsDraft('Mars/Olympus', [
        { days: [1], start: '00:00', end: '12:00', multiplier: '1' }
      ])
    ).toThrow('Unknown IANA time zone');

    expect(() => windowsDraft('UTC', [])).toThrow('at least one window period');

    expect(() =>
      windowsDraft('UTC', [
        { days: [], start: '00:00', end: '12:00', multiplier: '1' }
      ])
    ).toThrow('at least one day');

    expect(() =>
      windowsDraft('UTC', [
        { days: [1], start: '24:00', end: '24:00', multiplier: '1' }
      ])
    ).toThrow('starts before 24:00');

    expect(() =>
      windowsDraft('UTC', [
        { days: [1], start: '12:00', end: '12:00', multiplier: '1' }
      ])
    ).toThrow('ends after it starts');

    expect(() =>
      windowsDraft('UTC', [
        { days: [1], start: '08:00', end: '10:00', multiplier: '' }
      ])
    ).toThrow('needs a multiplier');

    const thirtyThree = Array.from({ length: 33 }, () => ({
      days: [1],
      start: '08:00',
      end: '10:00',
      multiplier: '1'
    }));
    expect(() => windowsDraft('UTC', thirtyThree)).toThrow(
      'At most 32 window periods'
    );
  });
});
