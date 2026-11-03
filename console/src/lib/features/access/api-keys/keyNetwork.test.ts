import { describe, expect, it } from 'vitest';
import { keyCIDRs, keyNetworkError } from './keyNetwork';

describe('API key network restrictions', () => {
  it('splits comma- and whitespace-separated ranges without blank entries', () => {
    expect(
      keyCIDRs(' 192.0.2.1/32,\n2001:db8::/32  198.51.100.0/24, ')
    ).toEqual(['192.0.2.1/32', '2001:db8::/32', '198.51.100.0/24']);
    expect(keyCIDRs(' ,  ')).toEqual([]);
    expect(keyNetworkError('')).toBeNull();
  });

  it.each([
    '0.0.0.0/0',
    '255.255.255.255/32',
    '192.0.2.1/24',
    '::/0',
    '2001:db8::1/128',
    '2001:DB8:0:0::/64'
  ])('accepts the valid range %s', (range) => {
    expect(keyNetworkError(range)).toBeNull();
  });

  it.each([
    '192.0.2.1',
    '192.0.2.1/',
    '192.0.2.1/-1',
    '192.0.2.1/33',
    '192.0.2.1/01',
    '192.0.2.1/32/1',
    '256.0.0.1/32',
    '192.00.2.1/24',
    '192.0.2/24',
    '2001:db8::1/129',
    '2001:db8::1::2/64',
    'fe80::1%eth0/64',
    '[2001:db8::1]/64',
    '::ffff:192.0.2.1/128',
    '::ffff:c000:201/128'
  ])('rejects the invalid or mapped range %s', (range) => {
    expect(keyNetworkError(range)).toContain('Use IPv4 or IPv6 CIDR ranges');
  });

  it('requires unique ranges and limits the number of restrictions', () => {
    const ranges = Array.from(
      { length: 65 },
      (_, index) => `192.0.2.${index}/32`
    );
    expect(keyNetworkError(ranges.slice(0, 64).join(','))).toBeNull();
    expect(keyNetworkError(ranges.join(','))).toBe(
      'Use at most 64 CIDR ranges.'
    );
    expect(keyNetworkError(`${ranges[0]}, ${ranges[0]}`)).toBe(
      'Use unique CIDR ranges.'
    );
  });
});
