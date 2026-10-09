export function keyCIDRs(value: string): string[] {
  return value
    .trim()
    .split(/[\s,]+/)
    .filter(Boolean);
}

export function keyNetworkError(value: string): string | null {
  const ranges = keyCIDRs(value);
  if (ranges.length > 64) return 'Use at most 64 CIDR ranges.';
  if (new Set(ranges).size !== ranges.length) return 'Use unique CIDR ranges.';
  for (const range of ranges) {
    const parts = range.split('/');
    const [address, bits] = parts;
    let valid = parts.length === 2 && /^(0|[1-9]\d{0,2})$/.test(bits ?? '');
    if (address.includes(':')) {
      try {
        const host = new URL(`http://[${address}]/`).hostname;
        valid &&=
          /^[0-9a-f:.]+$/i.test(address) &&
          Number(bits) <= 128 &&
          !/^\[::ffff:[\da-f]+:[\da-f]+\]$/.test(host);
      } catch {
        valid = false;
      }
    } else {
      const octets = address.split('.');
      valid &&=
        Number(bits) <= 32 &&
        octets.length === 4 &&
        octets.every(
          (part) => /^(0|[1-9]\d{0,2})$/.test(part) && Number(part) <= 255
        );
    }
    if (!valid)
      return 'Use IPv4 or IPv6 CIDR ranges, such as 192.0.2.0/24 or 2001:db8::/32. Express mapped IPv4 addresses as IPv4.';
  }
  return null;
}
