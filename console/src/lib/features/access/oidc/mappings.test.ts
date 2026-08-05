import { describe, expect, it } from 'vitest';
import { parseRoleMappings } from './mappings';

describe('parseRoleMappings', () => {
  it('accepts values containing equals signs and ignores blank lines', () => {
    expect(
      parseRoleMappings('team=platform=operator\n\nowner@example.com=owner')
    ).toEqual([
      { claim_value: 'team=platform', role: 'operator' },
      { claim_value: 'owner@example.com', role: 'owner' }
    ]);
  });

  it('rejects malformed mappings and unknown roles', () => {
    expect(() => parseRoleMappings('missing-role')).toThrow('claim-value=role');
    expect(() => parseRoleMappings('team=administrator')).toThrow(
      'invalid fixed role'
    );
  });
});
