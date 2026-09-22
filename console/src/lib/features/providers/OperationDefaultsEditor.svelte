<script lang="ts">
  import {
    nativeEntries,
    nativeObject,
    type NativePath
  } from '$lib/json/nativeJson';
  import type { ConfigurationDraft } from './configurationDraft.svelte';
  import { operationFields, type ProviderProfile } from './profiles';
  import NativeMapEditor from './NativeMapEditor.svelte';
  let {
    draft,
    profile,
    path,
    idPrefix,
    disabled = false,
    onChange
  }: {
    draft: ConfigurationDraft;
    profile: ProviderProfile;
    path: NativePath;
    idPrefix: string;
    disabled?: boolean;
    onChange: () => void;
  } = $props();
  let operation = $state('');
  const value = $derived(draft.at(path));
  const entries = $derived(nativeObject(value) ? nativeEntries(value) : []);
  const available = $derived(
    profile.operations.filter((name) => !entries.some(([key]) => key === name))
  );
  function add() {
    if (!operation) return;
    draft.set([...path, operation], {
      dialect: profile.operation_dialects[operation]!,
      values: {},
      native_options: {}
    });
    operation = '';
    onChange();
  }
</script>

<div class="defaults-editor">
  <p class="muted">
    Defaults fill omitted client fields. A client value, including native null,
    wins. Arrays and schema definitions replace as whole values.
  </p>
  {#if value !== undefined && !nativeObject(value)}<p
      class="inline-problem"
      role="alert"
    >
      Operation defaults must be a JSON object. Correct the advanced JSON.
    </p>{/if}
  {#each entries as [name, settings] (name)}
    <details open class="operation">
      <summary>{name} defaults</summary>
      {#if nativeObject(settings)}
        <p class="dialect">
          Dialect: <code
            >{draft.text([...path, name, 'dialect']) || 'not configured'}</code
          >
        </p>
        {#if draft.text( [...path, name, 'dialect'] ) !== profile.operation_dialects[name]}
          <p class="inline-problem" role="alert">
            This default set does not match the selected profile's operation
            dialect.
          </p>
          {#if profile.operation_dialects[name]}<button
              type="button"
              class="button button-secondary"
              {disabled}
              onclick={() => {
                draft.set(
                  [...path, name, 'dialect'],
                  profile.operation_dialects[name]!
                );
                onChange();
              }}>Use selected operation dialect</button
            >{/if}
        {/if}
        <NativeMapEditor
          {draft}
          path={[...path, name, 'values']}
          title={`${name} controls`}
          idPrefix={`${idPrefix}-${encodeURIComponent(name)}-controls`}
          fields={operationFields(profile, name)}
          {disabled}
          {onChange}
        />
        <NativeMapEditor
          {draft}
          path={[...path, name, 'native_options']}
          title={`${name} native options`}
          idPrefix={`${idPrefix}-${encodeURIComponent(name)}-native`}
          {disabled}
          {onChange}
        />
      {:else}<p class="inline-problem" role="alert">
          The default set must be an object with a dialect. Correct the advanced
          JSON or remove this set.
        </p>{/if}
      <button
        class="button button-secondary"
        type="button"
        {disabled}
        onclick={() => {
          draft.set([...path, name], undefined);
          onChange();
        }}>Remove {name} defaults</button
      >
    </details>
  {/each}
  <div class="add-operation">
    <div class="form-field">
      <label for={`${idPrefix}-operation`}>Operation for defaults</label><select
        id={`${idPrefix}-operation`}
        bind:value={operation}
        disabled={disabled || !available.length}
        ><option value="">Choose an operation</option
        >{#each available as name (name)}<option value={name}>{name}</option
          >{/each}</select
      >
    </div>
    <button
      class="button button-secondary"
      type="button"
      onclick={add}
      disabled={disabled ||
        !operation ||
        (value !== undefined && !nativeObject(value))}
      >Add operation defaults</button
    >
  </div>
</div>

<style>
  .muted,
  .dialect {
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .operation {
    margin: 1rem 0;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    padding: 1rem;
  }
  summary {
    cursor: pointer;
    font-weight: 500;
  }
  .add-operation {
    display: flex;
    gap: 0.75rem;
    align-items: flex-end;
    flex-wrap: wrap;
  }
  .add-operation .form-field {
    flex: 1 1 12rem;
  }
</style>
