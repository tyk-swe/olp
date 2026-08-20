import { describe, expect, it } from 'vitest';
import { parseRoleMappings } from './mappings';

describe('parseRoleMappings', () => {
  it('returns empty array for empty string or whitespace-only input', () => {
    expect(parseRoleMappings('')).toEqual([]);
    expect(parseRoleMappings('   \n  \n\t  ')).toEqual([]);
  });

  it('accepts values containing equals signs and ignores blank lines', () => {
    expect(
      parseRoleMappings('team=platform=operator\n\nowner@example.com=owner')
    ).toEqual([
      { claim_value: 'team=platform', role: 'operator' },
      { claim_value: 'owner@example.com', role: 'owner' }
    ]);
  });

  it('parses all valid fixed roles and trims extra whitespace', () => {
    const input = [
      '  admin_claim  =  owner  ',
      'ops_claim = operator',
      ' dev_claim = developer ',
      ' viewer_claim = viewer '
    ].join('\n');

    expect(parseRoleMappings(input)).toEqual([
      { claim_value: 'admin_claim', role: 'owner' },
      { claim_value: 'ops_claim', role: 'operator' },
      { claim_value: 'dev_claim', role: 'developer' },
      { claim_value: 'viewer_claim', role: 'viewer' }
    ]);
  });

  it('rejects malformed mappings missing claim value or equals sign', () => {
    expect(() => parseRoleMappings('missing-role')).toThrow('claim-value=role');
    expect(() => parseRoleMappings('=owner')).toThrow('claim-value=role');
  });

  it('rejects unknown or invalid fixed roles', () => {
    expect(() => parseRoleMappings('team=administrator')).toThrow(
      'invalid fixed role'
    );
    expect(() => parseRoleMappings('team=')).toThrow('invalid fixed role');
  });
});
