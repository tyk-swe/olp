import { describe, expect, it } from 'vitest';
import { attributionError, attributionForm, attributionInput } from './policy';
describe('attribution policy editor', () => {
  it('round-trips requirements and pins without mutating saved policy', () => {
    const saved = {
      required_attribution_keys: ['team'],
      attribution_defaults: { team: 'core' }
    };
    const form = attributionForm(saved);
    expect(attributionInput(form)).toEqual(saved);
    form.defaults[0].value = 'changed';
    expect(saved.attribution_defaults.team).toBe('core');
    expect(attributionInput(attributionForm())).toBeNull();
  });
  it('bounds the combined key set and rejects ambiguous or free-text labels', () => {
    const form = attributionForm({
      required_attribution_keys: ['a', 'b', 'c', 'd'],
      attribution_defaults: { e: 'fifth' }
    });
    expect(attributionError(form)).not.toBe('');
    form.required = 'team, team';
    form.defaults = [];
    expect(attributionError(form)).not.toBe('');
    form.required = 'team';
    form.defaults = [{ id: 'pin', key: 'team', value: 'my team' }];
    expect(attributionError(form)).not.toBe('');
    form.defaults[0].value = 'core';
    expect(attributionError(form)).toBe('');
  });
});
