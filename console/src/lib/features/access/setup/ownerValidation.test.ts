import { expect, it } from 'vitest';
import { validateOwner } from './ownerValidation';

it('accepts an email address without a dotted domain', () => {
  expect(
    validateOwner({
      displayName: 'Owner',
      installationName: '',
      email: 'a@localhost',
      password: 'a-valid-password',
      confirmPassword: 'a-valid-password',
      setupToken: 'token'
    })
  ).not.toHaveProperty('email');
});
