import { expect, it } from 'vitest';
import { PASSWORD_TOO_SHORT, validatePasswordPolicy } from './passwordPolicy';

it('counts code points when checking the minimum password length', () => {
  const sixEmoji = '😀'.repeat(6);
  expect(validatePasswordPolicy(sixEmoji, sixEmoji)).toEqual({
    password: PASSWORD_TOO_SHORT
  });
  const twelveEmoji = '😀'.repeat(12);
  expect(validatePasswordPolicy(twelveEmoji, twelveEmoji)).toEqual({});
});
