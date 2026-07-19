import { describe, expect, it } from 'vitest';
import { validateOwner } from './ownerValidation';

const validOwner = {
  displayName: 'Ada Owner',
  email: 'owner@example.com',
  password: 'correct horse battery staple',
  confirmPassword: 'correct horse battery staple',
  setupToken: 'bootstrap-token'
};

describe('owner setup validation', () => {
  it('accepts a complete owner account', () => {
    expect(validateOwner(validOwner)).toEqual({});
  });

  it('reports invalid identity fields and a short password together', () => {
    expect(
      validateOwner({
        displayName: '   ',
        email: 'not-an-address',
        password: 'short',
        confirmPassword: 'short',
        setupToken: ''
      })
    ).toEqual({
      displayName: 'Enter your name.',
      email: 'Enter a valid email address.',
      password: 'Use at least 12 characters.',
      setupToken: 'Enter the setup token.'
    });
  });

  it('attaches a password mismatch to the confirmation field', () => {
    expect(
      validateOwner({ ...validOwner, confirmPassword: 'a different secure phrase' })
    ).toEqual({ confirmPassword: 'Passwords do not match.' });
  });
});
