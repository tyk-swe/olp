import { mount, tick, unmount } from 'svelte';
import { describe, expect, it, vi } from 'vitest';
import type { ProviderModel } from '$lib/api/management/providers';
import CapabilityReview from './CapabilityReview.svelte';

const capability = { operation: 'generation', surface: 'open_ai', mode: 'sync' };

const missingModel: ProviderModel = {
  id: '00000000-0000-0000-0000-000000000001',
  upstream_model: 'missing-model',
  display_name: 'Missing model',
  enabled: true,
  inventory_source: 'upstream',
  availability: 'missing',
  capabilities: [{ ...capability, source: 'declared' }]
};

describe('CapabilityReview disable-only mode', () => {
  it('allows a missing enabled model to be disabled and saved without permitting edits or re-enabling', async () => {
    const onSave = vi.fn(async () => {});
    const component = mount(CapabilityReview, {
      target: document.body,
      props: {
        model: missingModel,
        options: [capability],
        disableOnly: true,
        onSave
      }
    });
    await tick();

    const eligibility = document.querySelector<HTMLInputElement>('input[type="checkbox"]')!;
    const save = [...document.querySelectorAll<HTMLButtonElement>('button')].find((button) =>
      button.textContent?.includes('Save capability review')
    )!;

    expect(eligibility.disabled).toBe(false);
    expect(save.disabled).toBe(false);
    expect([...document.querySelectorAll<HTMLSelectElement>('select')].every((select) => select.disabled)).toBe(true);

    eligibility.click();
    await tick();
    expect(eligibility.checked).toBe(false);
    expect(eligibility.disabled).toBe(true);

    save.click();
    await tick();
    expect(onSave).toHaveBeenCalledWith(false, [capability]);

    await unmount(component);
  });
});
