import { describe, expect, it } from 'vitest';
import {
  validateDisplayName,
  validateNewPassword,
  validatePassword
} from './validation';

describe('profile validation', () => {
  it('normalizes display names', () => {
    expect(validateDisplayName('  Ada Operator ')).toBe('Ada Operator');
  });

  it('requires a distinct confirmed password', () => {
    expect(() =>
      validatePassword(
        'old password here',
        'new password here',
        'different value'
      )
    ).toThrow('match');
    expect(
      validatePassword(
        'old password here',
        'new password here',
        'new password here'
      )
    ).toBe('new password here');
  });

  it('validates a confirmed password for first-time local enrollment', () => {
    expect(() => validateNewPassword('short', 'short')).toThrow(
      '12 characters'
    );
    expect(() =>
      validateNewPassword('local password for testing', 'different password')
    ).toThrow('match');
    expect(
      validateNewPassword(
        'local password for testing',
        'local password for testing'
      )
    ).toBe('local password for testing');
  });
});
