import { describe, expect, it } from 'vitest';
import { groupError, groupForm, groupInput, groupNamesError } from './policy';
describe('route group forms', () => {
  it('preserves empty groups and explicit membership', () => {
    const source = { empty: [], production: ['chat', 'coding'] };
    const form = groupForm(source);
    expect(groupError(form)).toBe('');
    expect(groupInput(form)).toEqual(source);
    form[1].routes = 'new';
    expect(source.production).toEqual(['chat', 'coding']);
    expect(groupInput([])).toEqual({});
  });
  it('rejects ambiguous names and non-slug members', () => {
    expect(groupNamesError(['same', 'same'])).not.toBe('');
    for (const routes of ['chat, chat', '../chat', 'Free text']) {
      expect(groupError([{ id: 'x', name: 'valid', routes }])).not.toBe('');
    }
  });
});
