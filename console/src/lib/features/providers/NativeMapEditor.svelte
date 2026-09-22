<script lang="ts">
  import {
    IncompleteJSON,
    nativeEntries,
    nativeObject,
    type NativePath
  } from '$lib/json/nativeJson';
  import type { ConfigurationDraft } from './configurationDraft.svelte';
  import type { FieldSchema } from './profiles';
  import NativeValueField from './NativeValueField.svelte';
  let {
    draft,
    path,
    title,
    idPrefix,
    fields,
    native = true,
    disabled = false,
    onChange
  }: {
    draft: ConfigurationDraft;
    path: NativePath;
    title: string;
    idPrefix: string;
    fields?: Record<string, FieldSchema>;
    native?: boolean;
    disabled?: boolean;
    onChange: () => void;
  } = $props();
  let name = $state('');
  let issue = $state('');
  const value = $derived(draft.at(path));
  const entries = $derived(nativeObject(value) ? nativeEntries(value) : []);
  const valid = $derived(value === undefined || nativeObject(value));
  const available = $derived(
    fields
      ? Object.keys(fields).filter(
          (key) => !entries.some(([name]) => name === key)
        )
      : undefined
  );
  function add() {
    issue = '';
    if (!name || !valid) {
      issue =
        'Choose a field name and correct the current object before adding a value.';
      return;
    }
    if (entries.some(([key]) => key === name)) {
      issue = 'That field already has a value.';
      return;
    }
    if (fields && !Object.hasOwn(fields, name)) {
      issue = 'Choose a field declared by the selected profile.';
      return;
    }
    draft.set(
      [...path, name],
      native
        ? new IncompleteJSON(
            '',
            'Enter a JSON value, or choose native null explicitly.'
          )
        : ''
    );
    name = '';
    onChange();
  }
</script>

<fieldset class="native-map" {disabled}>
  <legend>{title}</legend>
  {#if !valid}<p class="inline-problem" role="alert">
      This setting must be a JSON object. Correct the advanced JSON or remove
      it.
    </p>
    <button
      class="button button-secondary"
      type="button"
      onclick={() => {
        draft.set(path, undefined);
        onChange();
      }}>Remove invalid setting</button
    >
  {/if}
  {#each entries as [key] (key)}
    <NativeValueField
      {draft}
      path={[...path, key]}
      label={`${title}: ${key}`}
      id={`${idPrefix}-${encodeURIComponent(key)}`}
      schema={fields?.[key] ?? (native ? {} : { type: 'string' })}
      {native}
      {disabled}
      {onChange}
    />
  {/each}
  {#if available?.length === 0}<p class="muted">
      No additional fields are declared here by this profile.
    </p>
  {:else}
    <div class="add-row">
      <div class="form-field">
        <label for={`${idPrefix}-name`}>{title} field</label>
        {#if available}<select id={`${idPrefix}-name`} bind:value={name}
            ><option value="">Choose a field</option
            >{#each available as option (option)}<option value={option}
                >{fields?.[option]?.title ?? option}</option
              >{/each}</select
          >
        {:else}<input
            id={`${idPrefix}-name`}
            bind:value={name}
            autocomplete="off"
          />{/if}
      </div>
      <button
        class="button button-secondary"
        type="button"
        onclick={add}
        disabled={disabled || !valid}>Add {title.toLowerCase()} field</button
      >
    </div>
  {/if}
  {#if issue}<p class="inline-problem" role="alert">{issue}</p>{/if}
</fieldset>

<style>
  .native-map {
    border: 0;
    padding: 0;
    margin: 1.25rem 0;
    min-width: 0;
  }
  legend {
    font-weight: 500;
    margin-bottom: 0.75rem;
  }
  :global(.native-map .native-field) {
    margin-bottom: 1rem;
  }
  .add-row {
    display: flex;
    flex-wrap: wrap;
    gap: 0.65rem;
    align-items: flex-end;
  }
  .add-row .form-field {
    flex: 1 1 12rem;
  }
  .muted {
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
</style>
