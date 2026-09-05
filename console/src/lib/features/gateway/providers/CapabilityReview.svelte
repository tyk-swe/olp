<script lang="ts">
  import { guardUnsavedChanges } from '$lib/forms/unsavedChanges';
  import {
    type CapabilityCertification,
    type CapabilityDeclaration,
    type ProviderModel
  } from '$lib/api/management/providerModels';
  import {
    capabilityLimitReached,
    MAX_REVIEWED_CAPABILITIES
  } from './providerEditor';
  import ConflictNotice from '$lib/components/ConflictNotice.svelte';
  import { formatDate, stateLabel } from '$lib/format';
  import {
    beginReload,
    conflictNotice,
    initialConcurrentEdit,
    markDirty,
    markSaved,
    reconcile
  } from '$lib/forms/concurrentEdit';

  let {
    model,
    options,
    optionsPending = false,
    optionsError = false,
    disabled = false,
    reloadVersion = 0,
    providerEtag,
    certification = null,
    onSave
  }: {
    model: ProviderModel;
    options: CapabilityDeclaration[];
    optionsPending?: boolean;
    optionsError?: boolean;
    disabled?: boolean;
    reloadVersion?: number;
    providerEtag: string;
    certification?: CapabilityCertification | null;
    onSave: (
      enabled: boolean,
      capabilities: CapabilityDeclaration[],
      providerEtag: string
    ) => Promise<boolean>;
  } = $props();

  const operations = $derived([
    ...new Set(options.map((option) => option.operation))
  ]);

  let enabled = $state(false);
  let capabilities = $state<CapabilityDeclaration[]>([]);
  let sync = $state(initialConcurrentEdit());
  let localError = $state('');
  let observedReloadVersion = $state(0);
  let hydratedProviderEtag = $state('');
  const concurrentNotice = $derived(conflictNotice(sync));

  function signature(value: ProviderModel, etag: string) {
    return `${etag}:${value.id}:${value.enabled}:${value.capabilities.map((item) => `${item.operation}/${item.surface}/${item.mode}/${item.source}`).join(',')}`;
  }

  $effect(() => {
    if (reloadVersion !== observedReloadVersion) {
      observedReloadVersion = reloadVersion;
      sync = beginReload(sync);
    }
    const next = reconcile(sync, signature(model, providerEtag));
    if (next.state !== sync) sync = next.state;
    if (!next.hydrate) return;
    hydratedProviderEtag = providerEtag;
    enabled = model.enabled;
    capabilities = model.capabilities.map(({ operation, surface, mode }) => ({
      operation,
      surface,
      mode
    }));
  });

  function touch() {
    sync = markDirty(sync);
  }

  guardUnsavedChanges(() => sync.dirty);

  function surfacesFor(operation: string) {
    return [
      ...new Set(
        options
          .filter((option) => option.operation === operation)
          .map((option) => option.surface)
      )
    ];
  }

  function modesFor(capability: CapabilityDeclaration) {
    return options
      .filter(
        (option) =>
          option.operation === capability.operation &&
          option.surface === capability.surface
      )
      .map((option) => option.mode);
  }

  const limitReached = $derived(capabilityLimitReached(capabilities.length));
  const failedCertifications = $derived(
    certification?.results.filter((item) => !item.succeeded) ?? []
  );

  const unusedOptions = $derived(
    options.filter(
      (option) =>
        !capabilities.some(
          (existing) =>
            existing.operation === option.operation &&
            existing.surface === option.surface &&
            existing.mode === option.mode
        )
    )
  );

  function addCapability() {
    // The server refuses certification above this many reviewed tuples.
    if (limitReached) return;
    const capability =
      unusedOptions.find((option) => option.operation === 'generation') ??
      unusedOptions[0];
    if (!capability) return;
    capabilities = [...capabilities, capability];
    localError = '';
    touch();
  }

  function update(
    index: number,
    field: keyof CapabilityDeclaration,
    value: string
  ) {
    capabilities = capabilities.map((capability, itemIndex) => {
      if (itemIndex !== index) return capability;
      const updated = { ...capability, [field]: value };
      if (field === 'operation') {
        const next = options.find((option) => option.operation === value);
        if (next) {
          updated.surface = next.surface;
          updated.mode = next.mode;
        }
      } else if (field === 'surface') {
        const next = options.find(
          (option) =>
            option.operation === updated.operation && option.surface === value
        );
        if (next) updated.mode = next.mode;
      }
      return updated;
    });
    touch();
  }

  function remove(index: number) {
    capabilities = capabilities.filter((_, itemIndex) => itemIndex !== index);
    if (!capabilities.length) enabled = false;
    touch();
  }

  async function save() {
    localError = '';
    if (enabled && !capabilities.length) {
      localError =
        'Add at least one reviewed capability before enabling this model.';
      return;
    }
    const unique = new Set(
      capabilities.map(
        (item) => `${item.operation}/${item.surface}/${item.mode}`
      )
    );
    if (unique.size !== capabilities.length) {
      localError = 'Remove duplicate capability tuples.';
      return;
    }
    if (await onSave(enabled, capabilities, hydratedProviderEtag)) {
      // The parent refetched during onSave, so the props now carry the
      // post-save provider state; adopt it as the next save's baseline so a
      // second consecutive save does not send the stale ETag.
      hydratedProviderEtag = providerEtag;
      sync = markSaved(sync, signature(model, providerEtag));
    }
  }

  function reload() {
    sync = beginReload(sync);
  }
</script>

<div class="review">
  <div class="review-heading">
    <label class="enable"
      ><input
        type="checkbox"
        bind:checked={enabled}
        onchange={touch}
        {disabled}
      /> Eligible for routes</label
    >
    <button
      class="button button-secondary"
      type="button"
      onclick={addCapability}
      disabled={disabled || limitReached || !unusedOptions.length}
      >Add capability</button
    >
  </div>
  {#if limitReached}<p class="empty">
      This model carries the maximum of {MAX_REVIEWED_CAPABILITIES} reviewed tuples.
      Remove one before adding another.
    </p>{/if}
  {#if optionsPending}<p class="empty">
      Loading supported capability options…
    </p>{:else if optionsError}<p class="error" role="alert">
      Supported capability options could not be loaded.
    </p>{/if}
  {#if capabilities.length === 0}<p class="empty">
      No capabilities reviewed. This model cannot be enabled.
    </p>{/if}
  <div class="capability-list">
    {#each capabilities as capability, index (index)}
      <div class="capability-row">
        <label
          ><span class="sr-only">Operation {index + 1}</span><select
            value={capability.operation}
            onchange={(event) =>
              update(index, 'operation', event.currentTarget.value)}
            disabled={disabled || !options.length}
            >{#each operations as operation (operation)}<option
                value={operation}>{stateLabel(operation)}</option
              >{/each}</select
          ></label
        >
        <label
          ><span class="sr-only">Client surface {index + 1}</span><select
            value={capability.surface}
            onchange={(event) =>
              update(index, 'surface', event.currentTarget.value)}
            disabled={disabled || !options.length}
            >{#each surfacesFor(capability.operation) as surface (surface)}<option
                value={surface}
                >{surface === 'openai'
                  ? 'OpenAI'
                  : surface === 'anthropic'
                    ? 'Anthropic'
                    : 'Gemini'}</option
              >{/each}</select
          ></label
        >
        <label
          ><span class="sr-only">Mode {index + 1}</span><select
            value={capability.mode}
            onchange={(event) =>
              update(index, 'mode', event.currentTarget.value)}
            disabled={disabled || !options.length}
            >{#each modesFor(capability) as mode (mode)}<option value={mode}
                >{mode}</option
              >{/each}</select
          ></label
        >
        <button
          class="remove"
          type="button"
          aria-label={`Remove capability ${index + 1}`}
          onclick={() => remove(index)}
          {disabled}>×</button
        >
      </div>
    {/each}
  </div>
  {#if model.capabilities.length}
    <div class="evidence" aria-label="Stored capability evidence">
      {#each model.capabilities as capability (`${capability.operation}-${capability.surface}-${capability.mode}`)}
        <span class:certified={capability.source === 'certified'}
          ><code
            >{capability.operation}/{capability.surface}/{capability.mode}</code
          >
          · {capability.source}{#if capability.certified_at}
            · <time datetime={capability.certified_at}
              >{formatDate(capability.certified_at)}</time
            >{/if}</span
        >
      {/each}
    </div>
  {/if}
  {#if failedCertifications.length}
    <ul class="certification-results" aria-label="Failed certification tuples">
      {#each failedCertifications as item (`${item.operation}-${item.surface}-${item.mode}`)}<li
        >
          <code>{item.operation}/{item.surface}/{item.mode}</code>: {item.detail}{#if item.error_code}
            (<code>{item.error_code}</code>){/if}
        </li>{/each}
    </ul>
  {/if}
  {#if localError}<p class="error" role="alert">{localError}</p>{/if}
  <ConflictNotice notice={concurrentNotice} onReload={reload} {disabled} />
  <div class="review-footer">
    <span
      >Options are owned by the server. New tuples are stored with declared
      provenance; unchanged tuples keep their certification.</span
    >
    <button
      class="button button-secondary"
      type="button"
      onclick={save}
      {disabled}>Save capability review</button
    >
  </div>
</div>

<style>
  .review {
    display: grid;
    gap: 0.65rem;
    min-width: 28rem;
  }
  .review-heading,
  .review-footer {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.75rem;
  }
  .enable {
    display: flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.45rem;
    font-weight: 720;
  }
  .empty,
  .error {
    margin: 0;
    padding: 0.65rem;
    border-radius: 0.375rem;
    background: var(--surface-subtle);
    color: var(--foreground-muted);
    font-size: 0.75rem;
  }
  .error {
    background: var(--danger-soft);
    color: var(--danger);
  }
  .evidence {
    display: flex;
    flex-wrap: wrap;
    gap: 0.35rem;
  }
  .evidence span {
    padding: 0.3rem 0.45rem;
    border-radius: 0.25rem;
    background: var(--warning-soft);
    color: var(--warning);
    font-size: 0.68rem;
  }
  .evidence span.certified {
    background: var(--success-soft);
    color: var(--success);
  }
  .certification-results {
    margin: 0;
    padding-left: 1.25rem;
    list-style: disc;
    color: var(--danger);
    font-size: 0.8rem;
  }
  .capability-list {
    display: grid;
    gap: 0.4rem;
  }
  .capability-row {
    display: grid;
    grid-template-columns:
      minmax(8rem, 1.4fr) minmax(7rem, 1fr) minmax(6rem, 0.8fr)
      2.75rem;
    gap: 0.4rem;
  }
  select {
    width: 100%;
    min-height: 2.5rem;
    padding: 0.5rem;
    border: 1px solid var(--border-strong);
    border-radius: 0.375rem;
    background: var(--surface);
    color: var(--foreground);
  }
  .remove {
    width: 2.5rem;
    height: 2.5rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
    background: var(--surface);
    color: var(--danger);
    font-size: 1.2rem;
  }
  .review-footer {
    color: var(--foreground-muted);
    font-size: 0.72rem;
  }
  code {
    font-family: 'JetBrains Mono Variable', monospace;
  }
  @media (max-width: 58rem) {
    .review {
      min-width: 0;
    }
    .capability-row {
      grid-template-columns: 1fr 1fr;
    }
    .remove {
      justify-self: end;
    }
  }
  @media (max-width: 38rem) {
    .review-heading,
    .review-footer {
      display: grid;
    }
    .capability-row {
      grid-template-columns: 1fr 2.75rem;
    }
    .capability-row label {
      grid-column: 1 / -1;
    }
  }
</style>
