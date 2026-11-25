<script lang="ts">
  import { providerKeys } from './providerKeys.js';
  import { createQuery } from '@tanstack/svelte-query';
  import { listProviderVendors } from './providers.js';
  import {
    emptyProviderOptions,
    selectProviderPreset,
    setProviderDraftKind,
    type ProviderDraft
  } from './providerEditor.js';
  import type { ProviderKindCapability } from './models.js';

  let {
    draft,
    providerKinds,
    lockKind
  }: {
    draft: ProviderDraft;
    providerKinds: ProviderKindCapability[];
    lockKind: boolean;
  } = $props();

  const vendors = createQuery(() => ({
    queryKey: providerKeys.vendors(),
    queryFn: ({ signal }) => listProviderVendors(signal)
  }));
  // The last name this form suggested. A name the operator typed is never
  // replaced; only an untouched suggestion follows a later vendor choice.
  let suggestedName = $state('');

  function chooseVendor(event: Event) {
    const id = (event.currentTarget as HTMLSelectElement).value;
    const vendor = vendors.data?.find((vendor) => vendor.id === id);
    const kind = vendor?.connector ?? 'openai_compatible';
    const spec = providerKinds.find((spec) => spec.kind === kind);
    if (!spec) return;
    setProviderDraftKind(draft, kind);
    draft.authMode = spec.default_auth_mode;
    // The generic OpenAI-compatible vendor is a catalogue entry, not a preset;
    // it clears any previous preset's endpoint and keeps its vendor id.
    const preset = spec.presets.some((preset) => preset.id === id);
    if (kind === 'openai_compatible')
      selectProviderPreset(draft, spec, preset ? id : '');
    if (!preset) {
      draft.presetId = '';
      draft.options = { ...emptyProviderOptions(), vendor_id: id || null };
    }
    if (vendor && (!draft.name.trim() || draft.name === suggestedName)) {
      draft.name = vendor.name;
      suggestedName = vendor.name;
    }
  }

  function chooseProviderKind(event: Event) {
    const kind = (event.currentTarget as HTMLInputElement)
      .value as ProviderDraft['kind'];
    setProviderDraftKind(draft, kind);
    const spec = providerKinds.find((candidate) => candidate.kind === kind);
    if (spec) draft.authMode = spec.default_auth_mode;
  }
</script>

<fieldset disabled={lockKind}>
  <legend>Choose a vendor</legend>
  <div class="form-field">
    <label for="provider-vendor">Vendor</label><select
      id="provider-vendor"
      value={draft.presetId || draft.options?.vendor_id || ''}
      onchange={chooseVendor}
    >
      <option value="">
        Custom connection
      </option>{#each vendors.data ?? [] as vendor (vendor.id)}<option
          value={vendor.id}
        >
          {vendor.name}
        </option>{/each}
    </select><small>
      Each connection can have its own account, endpoint, region, and credential
      pool.
    </small>
  </div>
  {#if vendors.isError}<div class="inline-problem" role="alert">
      Vendor catalog unavailable. <button
        class="button button-secondary"
        type="button"
        onclick={() => vendors.refetch()}
      >
        Retry
      </button>
    </div>{/if}
  <p class="connector-label">Connector protocol</p>
  <div class="connector-grid">
    {#each providerKinds as option (option.kind)}
      <label class:selected={draft.kind === option.kind}>
        <input
          type="radio"
          name="kind"
          value={option.kind}
          checked={draft.kind === option.kind}
          onchange={chooseProviderKind}
        />
        <strong>{option.label}</strong><small>{option.description}</small>
      </label>
    {/each}
  </div>
  {#if lockKind}
    <p class="connector-locked">
      The connector kind is fixed once the provider draft exists. Delete this
      draft and start again to use a different connector.
    </p>
  {/if}
</fieldset>

<style>
  fieldset {
    margin: 0 0 1.5rem;
    padding: 0;
    border: 0;
  }
  legend {
    margin: 0 0 0.75rem;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .connector-label {
    margin: 1.1rem 0 0.45rem;
    font-weight: 500;
  }
  .connector-locked {
    margin: 0.6rem 0 0;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .connector-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.6rem;
  }
  .connector-grid label {
    position: relative;
    display: grid;
    min-height: 5.6rem;
    align-content: center;
    gap: 0.2rem;
    padding: 0.8rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    transition:
      border-color var(--motion),
      background-color var(--motion),
      box-shadow var(--motion);
  }
  .connector-grid label:hover {
    border-color: var(--border-strong);
  }
  .connector-grid label:has(input:focus-visible) {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  .connector-grid label.selected {
    border-color: var(--signal);
    background: var(--accent-soft);
    box-shadow: inset 0 0 0 1px var(--signal);
  }
  .connector-grid input {
    position: absolute;
    opacity: 0;
  }
  .connector-grid small {
    color: var(--foreground-muted);
  }
  @media (max-width: 64rem) {
    .connector-grid {
      grid-template-columns: repeat(2, 1fr);
    }
  }
  @media (max-width: 42rem) {
    .connector-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
