<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { apiClient } from '$lib/api/client';
  import { result } from '$lib/api/http';
  import ProviderConnectionFields from './ProviderConnectionFields.svelte';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import type { ProviderKindCapability } from '$lib/features/providers/models';
  import {
    emptyProviderOptions,
    requiresCredential,
    requiresSeedModel,
    selectProviderPreset,
    setProviderDraftKind,
    type ProviderDraft
  } from '$lib/features/providers/providerEditor';

  let {
    draft = $bindable(),
    providerKinds,
    selectedSpec,
    busy,
    lockKind = false,
    onSubmit
  }: {
    draft: ProviderDraft;
    providerKinds: ProviderKindCapability[];
    selectedSpec: ProviderKindCapability;
    busy: string;
    /** Set once the draft provider exists; its connector kind is immutable. */
    lockKind?: boolean;
    onSubmit: (event: SubmitEvent) => void | Promise<void>;
  } = $props();

  const vendors = createQuery(() => ({
    queryKey: ['provider-vendors'],
    queryFn: async () => {
      const response = await apiClient.GET('/api/v3/provider-vendors');
      return result(response.data, response.error, response.response);
    }
  }));
  function chooseVendor(event: Event) {
    const id = (event.currentTarget as HTMLSelectElement).value;
    const vendor = vendors.data?.find((vendor) => vendor.id === id);
    const kind = vendor?.connector ?? 'openai_compatible';
    const spec = providerKinds.find((spec) => spec.kind === kind);
    if (!spec) return;
    setProviderDraftKind(draft, kind);
    draft.authMode = spec.default_auth_mode;
    if (kind === 'openai_compatible') selectProviderPreset(draft, spec, id);
    else {
      draft.presetId = '';
      draft.options = { ...emptyProviderOptions(), vendor_id: id || null };
    }
  }

  const credentialRequired = $derived(
    requiresCredential(selectedSpec, draft.authMode)
  );
  const seedModelRequired = $derived(
    requiresSeedModel(selectedSpec) ||
      ['voyage', 'perplexity', 'cohere'].includes(draft.presetId)
  );
  const selectedPreset = $derived(
    selectedSpec.presets.find((preset) => preset.id === draft.presetId)
  );

  function chooseProviderKind(event: Event) {
    setProviderDraftKind(
      draft,
      (event.currentTarget as HTMLInputElement).value as ProviderDraft['kind']
    );
  }

  function chooseCompatibleProvider(event: Event) {
    selectProviderPreset(
      draft,
      selectedSpec,
      (event.currentTarget as HTMLSelectElement).value
    );
  }
</script>

<form
  class="card editor"
  aria-busy={Boolean(busy)}
  onsubmit={onSubmit}
  novalidate
>
  <fieldset disabled={lockKind}>
    <legend>Choose a vendor</legend>
    <div class="form-field">
      <label for="provider-vendor">Vendor</label><select
        id="provider-vendor"
        value={draft.presetId || draft.options?.vendor_id || ''}
        onchange={chooseVendor}
        ><option value="">Custom connection</option
        >{#each vendors.data ?? [] as vendor (vendor.id)}<option
            value={vendor.id}>{vendor.name}</option
          >{/each}</select
      ><small
        >Each connection can have its own account, endpoint, region, and
        credential pool.</small
      >
    </div>
    {#if vendors.isError}<div class="inline-problem" role="alert">
        Vendor catalog unavailable. <button
          class="button button-secondary"
          type="button"
          onclick={() => vendors.refetch()}>Retry</button
        >
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
  <div class="form-grid">
    <ProviderConnectionFields
      bind:values={draft}
      spec={selectedSpec}
      idPrefix="provider"
      authEditable
      endpointReadonly={false}
    />
    {#if draft.kind === 'openai_compatible'}<div class="form-field full">
        <label for="compatible-provider">Compatible provider</label><select
          id="compatible-provider"
          value={draft.presetId}
          onchange={chooseCompatibleProvider}
        >
          <option value="">Custom endpoint</option>
          {#each selectedSpec.presets as preset (preset.id)}
            <option value={preset.id}>{preset.label}</option>
          {/each}
        </select><small
          >Presets fill reviewed connector values. Custom endpoint preserves the
          fully manual path.</small
        >
      </div>
      {#if selectedPreset}<div class="preset-note full" aria-live="polite">
          <strong>{selectedPreset.label}</strong>
          <span>{selectedPreset.description}</span>
          <code>{selectedPreset.endpoint}</code>
          <span
            >Maintained by {selectedPreset.maintainer}. Verified against
            <a
              href={selectedPreset.documentation_url}
              target="_blank"
              rel="noreferrer noopener">{selectedPreset.documentation_label}</a
            >.</span
          >
        </div>{/if}{/if}
    <div class="form-field">
      <label for="initial-model"
        >{seedModelRequired
          ? draft.kind === 'vertex_ai'
            ? 'Vertex probe model'
            : 'Probe model'
          : 'Seed model (optional)'}</label
      ><input
        id="initial-model"
        aria-describedby="initial-model-help"
        autocomplete="off"
        bind:value={draft.model}
        placeholder={seedModelRequired
          ? draft.kind === 'vertex_ai'
            ? 'publishers/google/models/gemini-2.5-pro'
            : 'Exact upstream model ID'
          : 'gpt-5.4'}
        required={seedModelRequired}
      /><small id="initial-model-help"
        >{seedModelRequired
          ? 'This provider needs an explicit model to test credentials.'
          : 'Used for the initial connector probe; upstream discovery follows.'}</small
      >
    </div>
    {#if draft.authMode === 'headers'}<label class="form-field full"
        >Credential header names<input
          bind:value={draft.credentialHeaders}
          placeholder="authorization, x-account-id"
        /><small
          >Supply a JSON object with these names and their secret values in
          Credential.</small
        ></label
      >{/if}
    {#if credentialRequired}<div class="form-field full">
        <label for="provider-secret">Credential</label><input
          id="provider-secret"
          aria-describedby="credential-help"
          type="password"
          autocomplete="new-password"
          bind:value={draft.credential}
          required
        /><small id="credential-help"
          >Sent once to this installation; never saved by the console or
          returned by the API.</small
        >
      </div>{:else}<div class="identity-note full">
        <strong>No stored credential</strong><span
          >{draft.authMode === 'none'
            ? 'No authentication will be sent to this endpoint.'
            : 'This provider uses the workload identity available to the OLP process.'}</span
        >
      </div>{/if}
  </div>
  <div class="form-actions">
    <button class="button button-primary" type="submit" disabled={Boolean(busy)}
      >{busy === 'create' ? 'Saving and testing…' : 'Save and test connection'}
      <NavIcon name="arrow" /></button
    >
  </div>
</form>

<style>
  .connector-locked {
    margin: 0.6rem 0 0;
    color: var(--foreground-muted);
    font-size: 0.78rem;
  }

  .editor {
    padding: clamp(1.15rem, 3vw, 1.75rem);
  }
  fieldset {
    margin: 0 0 1.5rem;
    padding: 0;
    border: 0;
  }
  legend {
    margin: 0 0 0.85rem;
    font-size: 1.15rem;
    font-weight: 750;
    letter-spacing: -0.025em;
  }
  .connector-label {
    margin: 1.1rem 0 0.45rem;
    font-weight: 700;
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
    border-radius: 0.375rem;
  }
  .connector-grid label:has(input:focus-visible) {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  .connector-grid label.selected {
    border-color: var(--accent);
    background: var(--accent-soft);
  }
  .connector-grid input {
    position: absolute;
    opacity: 0;
  }
  .connector-grid small {
    color: var(--foreground-muted);
  }
  .form-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.65rem;
    margin-top: 1.35rem;
  }
  .identity-note {
    display: grid;
    gap: 0.15rem;
    padding: 0.8rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
    background: var(--surface-subtle);
    color: var(--foreground-muted);
    font-size: 0.78rem;
  }
  .identity-note strong {
    color: var(--foreground);
  }
  .identity-note.full {
    grid-column: 1 / -1;
  }
  .preset-note {
    display: grid;
    gap: 0.3rem;
    padding: 0.85rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
    background: var(--surface-subtle);
    color: var(--foreground-muted);
    font-size: 0.78rem;
  }
  .preset-note strong {
    color: var(--foreground);
    font-size: 0.9rem;
  }
  .preset-note code {
    color: var(--foreground);
  }
  .preset-note a {
    color: var(--accent-strong);
    font-weight: 700;
  }
  code {
    font:
      0.75rem 'JetBrains Mono Variable',
      monospace;
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
