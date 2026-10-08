import { describe, expect, it } from 'vitest';
import { keyCIDRs, keyNetworkError } from './keyNetwork';

describe('key network restrictions', () => {
  it('loads IPv4 and IPv6 ranges and allows explicit clearing', () => {
    const text = '192.0.2.0/24,\n2001:db8::/32';
    expect(keyNetworkError(text)).toBeNull();
    expect(keyCIDRs(text)).toEqual(['192.0.2.0/24', '2001:db8::/32']);
    expect(keyCIDRs(' ')).toEqual([]);
    expect(keyNetworkError('')).toBeNull();
    expect(keyNetworkError('0.0.0.0/0\n::/0')).toBeNull();
    expect(keyNetworkError('::ffff:0:c000:201/128')).toBeNull();
  });

  it.each([
    'localhost',
    '192.0.2.0/33',
    '256.0.0.0/8',
    '192.00.2.0/24',
    '::/129',
    '::gg/64',
    '::ffff:192.0.2.1/128',
    '192.0.2.0/24/1',
    '192.0.2.0/24/',
    '::1]#fragment/128',
    '192.0.2.0/24\n192.0.2.0/24'
  ])('rejects invalid range %s', (value) => {
    expect(keyNetworkError(value)).not.toBeNull();
  });
});
