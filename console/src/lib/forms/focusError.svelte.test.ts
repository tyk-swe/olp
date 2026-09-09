import { afterEach, describe, expect, it } from 'vitest';
import { focusFormError, focusErrorSummary } from './focusError';
import { tick } from 'svelte';

afterEach(() => document.body.replaceChildren());

describe('validation focus', () => {
  it('focuses the first editable invalid input without changing entered values', async () => {
    document.body.innerHTML =
      '<main><input value="retained"><input aria-invalid="true" disabled><input id="invalid" aria-invalid="true" value="0"><div data-error-summary tabindex="-1">Check your input</div></main>';
    await focusFormError(document.querySelector('main')!);
    expect(document.activeElement?.id).toBe('invalid');
    expect(document.querySelector('input')?.value).toBe('retained');
  });

  it('focuses a summary when no field is identified', async () => {
    document.body.innerHTML =
      '<main><div id="summary" data-error-summary tabindex="-1">Choose a scope</div></main>';
    await focusFormError(document.querySelector('main')!);
    expect(document.activeElement?.id).toBe('summary');
  });

  it('does not steal focus after an error is removed', async () => {
    document.body.innerHTML =
      '<button>Continue</button><div tabindex="-1">Failed</div>';
    const button = document.querySelector('button')!;
    button.focus();
    const action = focusErrorSummary(document.querySelector('div')!);
    action.destroy();
    await tick();
    expect(document.activeElement).toBe(button);
  });
});
