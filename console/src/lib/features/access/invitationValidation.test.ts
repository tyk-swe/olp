import { describe, expect, it } from 'vitest';
import { validateInvitationAcceptance } from './invitationValidation';

const valid = {
  displayName: 'Grace Operator',
  password: 'correct horse battery staple',
  confirmPassword: 'correct horse battery staple'
};

describe('invitation acceptance validation', () => {
  it('accepts a complete invited-user profile', () => {
    expect(validateInvitationAcceptance(valid)).toEqual({});
  });

  it('reports every actionable field error', () => {
    expect(
      validateInvitationAcceptance({ displayName: ' ', password: 'short', confirmPassword: 'no' })
    ).toEqual({
      displayName: 'Enter your display name.',
      password: 'Use at least 12 characters.'
    });
  });

  it('attaches a valid-password mismatch to confirmation', () => {
    expect(
      validateInvitationAcceptance({ ...valid, confirmPassword: 'another secure password' })
    ).toEqual({ confirmPassword: 'Passwords do not match.' });
  });
});
