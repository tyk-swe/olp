import { expect, it } from 'vitest';
import {
  DISPLAY_NAME_TOO_LONG,
  validateDisplayName
} from './displayNamePolicy';

it('counts code points when checking the maximum display name length', () => {
  expect(validateDisplayName('😀'.repeat(51))).toBeUndefined();
  expect(validateDisplayName('😀'.repeat(101))).toBe(DISPLAY_NAME_TOO_LONG);
});
