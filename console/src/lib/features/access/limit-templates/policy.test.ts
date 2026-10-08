import { expect, it } from 'vitest';
import { templateForm, templateInput, templateError } from './policy';
it('round-trips precise template ceilings and rejects duplicate names', () => {
  const saved = { standard: { daily_cost_limit: '0.000000000001' } };
  const form = templateForm(saved);
  expect(templateInput(form).standard.daily_cost_limit).toBe('0.000000000001');
  expect(templateError(form)).toBe('');
  form.push({ ...form[0], id: 'copy' });
  expect(templateError(form)).toContain('unique');
  expect(saved.standard.daily_cost_limit).toBe('0.000000000001');
  expect(templateInput([])).toEqual({});
});
