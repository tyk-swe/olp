<script lang="ts">
  import {
    type CapabilityDeclaration,
    type ProviderModel
  } from '$lib/api/management/providers';

  let {
    model,
    options,
    optionsPending = false,
    optionsError = false,
    disabled = false,
    disableOnly = false,
    onSave
  }: {
    model: ProviderModel;
    options: CapabilityDeclaration[];
    optionsPending?: boolean;
    optionsError?: boolean;
    disabled?: boolean;
    disableOnly?: boolean;
    onSave: (enabled: boolean, capabilities: CapabilityDeclaration[]) => Promise<void>;
  } = $props();

  const operations = $derived([...new Set(options.map((option) => option.operation))]);

  let enabled = $state(false);
  let capabilities = $state<CapabilityDeclaration[]>([]);
  let initialized = $state('');
  let localError = $state('');

  $effect(() => {
    const signature = `${model.id}:${model.enabled}:${model.capabilities.map((item) => `${item.operation}/${item.surface}/${item.mode}/${item.source}`).join(',')}`;
    if (signature === initialized) return;
    initialized = signature;
    enabled = model.enabled;
    capabilities = model.capabilities.map(({ operation, surface, mode }) => ({ operation, surface, mode }));
  });

  function surfacesFor(operation: string) {
    return [...new Set(options.filter((option) => option.operation === operation).map((option) => option.surface))];
  }

  function modesFor(capability: CapabilityDeclaration) {
    return options
      .filter((option) => option.operation === capability.operation && option.surface === capability.surface)
      .map((option) => option.mode);
  }

  function addCapability() {
    const capability = options.find((option) => option.operation === 'generation') ?? options[0];
    if (!capability) return;
    capabilities = [...capabilities, capability];
    localError = '';
  }

  function update(index: number, field: keyof CapabilityDeclaration, value: string) {
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
          (option) => option.operation === updated.operation && option.surface === value
        );
        if (next) updated.mode = next.mode;
      }
      return updated;
    });
  }

  function remove(index: number) {
    capabilities = capabilities.filter((_, itemIndex) => itemIndex !== index);
    if (!capabilities.length) enabled = false;
  }

  async function save() {
    localError = '';
    if (enabled && !capabilities.length) {
      localError = 'Add at least one reviewed capability before enabling this model.';
      return;
    }
    const unique = new Set(capabilities.map((item) => `${item.operation}/${item.surface}/${item.mode}`));
    if (unique.size !== capabilities.length) {
      localError = 'Remove duplicate capability tuples.';
      return;
    }
    await onSave(enabled, capabilities);
  }
</script>

<div class="review">
  <div class="review-heading">
    <label class="enable"><input type="checkbox" bind:checked={enabled} disabled={disabled || (disableOnly && !enabled)} /> Eligible for routes</label>
    <button class="button button-secondary" type="button" onclick={addCapability} disabled={disabled || disableOnly || !options.length}>Add capability</button>
  </div>
  {#if optionsPending}<p class="empty">Loading supported capability options…</p>{:else if optionsError}<p class="error" role="alert">Supported capability options could not be loaded.</p>{/if}
  {#if capabilities.length === 0}<p class="empty">No capabilities reviewed. This model cannot be enabled.</p>{/if}
  <div class="capability-list">
    {#each capabilities as capability, index (index)}
      <div class="capability-row">
        <label><span class="sr-only">Operation {index + 1}</span><select value={capability.operation} onchange={(event) => update(index, 'operation', event.currentTarget.value)} disabled={disabled || disableOnly || !options.length}>{#each operations as operation (operation)}<option value={operation}>{operation.replaceAll('_', ' ')}</option>{/each}</select></label>
        <label><span class="sr-only">Client surface {index + 1}</span><select value={capability.surface} onchange={(event) => update(index, 'surface', event.currentTarget.value)} disabled={disabled || disableOnly || !options.length}>{#each surfacesFor(capability.operation) as surface (surface)}<option value={surface}>{surface === 'open_ai' ? 'OpenAI' : surface === 'anthropic' ? 'Anthropic' : 'Gemini'}</option>{/each}</select></label>
        <label><span class="sr-only">Mode {index + 1}</span><select value={capability.mode} onchange={(event) => update(index, 'mode', event.currentTarget.value)} disabled={disabled || disableOnly || !options.length}>{#each modesFor(capability) as mode (mode)}<option value={mode}>{mode}</option>{/each}</select></label>
        <button class="remove" type="button" aria-label={`Remove capability ${index + 1}`} onclick={() => remove(index)} disabled={disabled || disableOnly}>×</button>
      </div>
    {/each}
  </div>
  {#if model.capabilities.length}
    <div class="evidence" aria-label="Stored capability evidence">
      {#each model.capabilities as capability (`${capability.operation}-${capability.surface}-${capability.mode}`)}
        <span class:certified={capability.source === 'certified'}><code>{capability.operation}/{capability.surface}/{capability.mode}</code> · {capability.source}{#if capability.certified_at} · <time datetime={capability.certified_at}>{new Date(capability.certified_at).toLocaleString()}</time>{/if}</span>
      {/each}
    </div>
  {/if}
  {#if localError}<p class="error" role="alert">{localError}</p>{/if}
  <div class="review-footer">
    <span>Options are owned by the server. Operator-reviewed tuples are stored with declared provenance.</span>
    <button class="button button-secondary" type="button" onclick={save} disabled={disabled}>Save capability review</button>
  </div>
</div>

<style>
  .review { display: grid; gap: .65rem; min-width: 28rem; }
  .review-heading, .review-footer { display: flex; align-items: center; justify-content: space-between; gap: .75rem; }
  .enable { display: flex; min-height: 2.75rem; align-items: center; gap: .45rem; font-weight: 720; }
  .empty, .error { margin: 0; padding: .65rem; border-radius: .375rem; background: var(--surface-subtle); color: var(--foreground-muted); font-size: .75rem; }
  .error { background: var(--danger-soft); color: var(--danger); }
  .evidence { display: flex; flex-wrap: wrap; gap: .35rem; }
  .evidence span { padding: .3rem .45rem; border-radius: .25rem; background: var(--warning-soft); color: var(--warning); font-size: .68rem; }
  .evidence span.certified { background: var(--success-soft); color: var(--success); }
  .capability-list { display: grid; gap: .4rem; }
  .capability-row { display: grid; grid-template-columns: minmax(8rem, 1.4fr) minmax(7rem, 1fr) minmax(6rem, .8fr) 2.75rem; gap: .4rem; }
  select { width: 100%; min-height: 2.5rem; padding: .5rem; border: 1px solid var(--border-strong); border-radius: .375rem; background: var(--surface); color: var(--foreground); }
  .remove { width: 2.5rem; height: 2.5rem; border: 1px solid var(--border); border-radius: .375rem; background: var(--surface); color: var(--danger); font-size: 1.2rem; }
  .review-footer { color: var(--foreground-muted); font-size: .72rem; }
  code { font-family: 'JetBrains Mono Variable', monospace; }
  @media (max-width: 58rem) { .review { min-width: 0; } .capability-row { grid-template-columns: 1fr 1fr; } .remove { justify-self: end; } }
  @media (max-width: 38rem) { .review-heading, .review-footer { display: grid; } .capability-row { grid-template-columns: 1fr 2.75rem; } .capability-row label { grid-column: 1 / -1; } }
</style>
