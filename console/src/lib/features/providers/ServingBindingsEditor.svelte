<script lang="ts">
  import { nativeEntries, nativeObject } from '$lib/json/nativeJson';
  import type { ConfigurationDraft } from './configurationDraft.svelte';
  import type { ProviderProfile, FieldSchema } from './profiles';
  import NativeValueField from './NativeValueField.svelte';
  import OperationDefaultsEditor from './OperationDefaultsEditor.svelte';
  let {
    draft,
    profile,
    schema,
    idPrefix,
    disabled = false,
    onChange
  }: {
    draft: ConfigurationDraft;
    profile: ProviderProfile;
    schema: FieldSchema;
    idPrefix: string;
    disabled?: boolean;
    onChange: () => void;
  } = $props();
  let selected = $state('');
  let name = $state('');
  let issue = $state('');
  const value = $derived(draft.at(['options', 'bindings']));
  const entries = $derived(nativeObject(value) ? nativeEntries(value) : []);
  const active = $derived(
    entries.some(([key]) => key === selected)
      ? selected
      : (entries[0]?.[0] ?? '')
  );
  const binding = $derived(
    active ? draft.at(['options', 'bindings', active]) : undefined
  );
  const fields = $derived(
    Object.entries(schema.properties ?? {}).filter(
      ([key]) => key !== 'defaults'
    )
  );
  function add() {
    if (!name) {
      issue = 'Enter the configured upstream model name.';
      return;
    }
    if (entries.some(([key]) => key === name)) {
      issue = 'That model already has a binding.';
      return;
    }
    draft.set(['options', 'bindings', name], {});
    selected = name;
    name = issue = '';
    onChange();
  }
  function label(name: string) {
    return name
      .replaceAll('_', ' ')
      .replace(/^./, (letter) => letter.toUpperCase());
  }
</script>

<p class="muted">
  Bindings belong to configured upstream model names. Principal, snapshot,
  region and resource scope are declarations until independently observed.
</p>
{#if entries.length}<div class="form-field">
    <label for={`${idPrefix}-selected`}>Edit model binding</label><select
      id={`${idPrefix}-selected`}
      value={active}
      onchange={(event) => (selected = event.currentTarget.value)}
      {disabled}
      >{#each entries as [key] (key)}<option value={key}>{key}</option
        >{/each}</select
    >
  </div>{/if}
{#if active}
  {#if nativeObject(binding)}
    <div class="form-grid">
      {#each fields as [field, definition] (field)}<NativeValueField
          {draft}
          path={['options', 'bindings', active, field]}
          label={`Binding ${label(field)}`}
          id={`${idPrefix}-${encodeURIComponent(active)}-${field}`}
          schema={definition}
          {disabled}
          {onChange}
        />{/each}
    </div>
    <OperationDefaultsEditor
      {draft}
      {profile}
      path={['options', 'bindings', active, 'defaults']}
      idPrefix={`${idPrefix}-${encodeURIComponent(active)}-defaults`}
      {disabled}
      {onChange}
    />
  {:else}<p class="inline-problem" role="alert">
      This binding must be a JSON object. Correct the advanced JSON or remove
      it.
    </p>{/if}
  <button
    class="button button-secondary"
    type="button"
    {disabled}
    onclick={() => {
      draft.set(['options', 'bindings', active], undefined);
      onChange();
    }}>Remove model binding</button
  >
{/if}
<div class="add-binding">
  <div class="form-field">
    <label for={`${idPrefix}-name`}>New upstream model binding</label><input
      id={`${idPrefix}-name`}
      bind:value={name}
      {disabled}
    />
  </div>
  <button
    class="button button-secondary"
    type="button"
    onclick={add}
    disabled={disabled || (value !== undefined && !nativeObject(value))}
    >Add model binding</button
  >
</div>
{#if issue}<p class="inline-problem" role="alert">{issue}</p>{/if}

<style>
  .muted {
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .add-binding {
    display: flex;
    gap: 0.75rem;
    align-items: flex-end;
    flex-wrap: wrap;
    margin-top: 1.25rem;
  }
  .add-binding .form-field {
    flex: 1 1 15rem;
  }
  .form-grid {
    margin-top: 1rem;
  }
</style>
