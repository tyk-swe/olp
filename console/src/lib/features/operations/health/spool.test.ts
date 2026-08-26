import { describe, expect, it } from 'vitest';
import { spoolUsage } from './spool';

describe('spoolUsage', () => {
  it('reports an unmeasured volume instead of an empty one', () => {
    expect(spoolUsage(null, null)).toBe('—');
    expect(spoolUsage(undefined, undefined)).toBe('—');
    expect(spoolUsage(0, null)).toBe('—');
    expect(spoolUsage(null, 1024)).toBe('—');
  });

  it('names a spool with no capacity rather than dividing by zero', () => {
    expect(spoolUsage(0, 0)).toBe('0 B of 0 B (no capacity)');
  });

  it('states used, total, and share in binary units', () => {
    expect(spoolUsage(536_870_912, 1_073_741_824)).toBe('512 MiB of 1.00 GiB (50.0%)');
  });
});
