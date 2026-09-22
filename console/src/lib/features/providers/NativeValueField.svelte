<script lang="ts">
  import { IncompleteJSON, type NativePath } from '$lib/json/nativeJson';
  import type { ConfigurationDraft } from './configurationDraft.svelte';
  import type { FieldSchema } from './profiles';
  let {
    draft,
    path,
    label,
    id,
    schema = {},
    disabled = false,
    native = false,
    onChange
  }: {
    draft: ConfigurationDraft;
    path: NativePath;
    label: string;
    id: string;
    schema?: FieldSchema;
    disabled?: boolean;
    native?: boolean;
    onChange: () => void;
  } = $props();
  let actionIssue = $state('');
  const value = $derived(draft.at(path));
  const present = $derived(value !== undefined);
  const issue = $derived(
    actionIssue || (value instanceof IncompleteJSON ? value.issue : '')
  );
  const stringField = $derived(schema.type === 'string' && !native);
  const multiline = $derived(stringField && path.at(-1) === 'trust_roots_pem');
  const source = $derived(
    stringField ? (typeof value === 'string' ? value : '') : draft.json(path)
  );
  function edit(source: string) {
    actionIssue = '';
    try {
      if (stringField) draft.set(path, source);
      else draft.setJSON(path, source);
      onChange();
    } catch (error) {
      actionIssue =
        error instanceof Error
          ? error.message
          : 'Correct the advanced configuration first.';
    }
  }
  function remove() {
    draft.set(path, undefined);
    onChange();
  }
  function setNull() {
    draft.set(path, null);
    onChange();
  }
</script>

<div class="form-field native-field">
  <div class="field-heading">
    <label for={id}>{label}</label>
    <span class="presence"
      >{present
        ? value === null
          ? 'Native null'
          : 'Configured'
        : 'Not configured'}</span
    >
  </div>
  {#if multiline}
    <textarea
      {id}
      value={source}
      oninput={(event) => edit(event.currentTarget.value)}
      rows="4"
      spellcheck="false"
      {disabled}
      aria-describedby={`${id}-help`}></textarea>
  {:else if native}
    <textarea
      {id}
      value={source}
      oninput={(event) => edit(event.currentTarget.value)}
      rows={source.length > 100 || source.includes('\n') ? 5 : 2}
      spellcheck="false"
      {disabled}
      aria-invalid={Boolean(issue)}
      aria-describedby={`${id}-help`}></textarea>
  {:else}
    <input
      {id}
      type={schema.format === 'uri' ? 'url' : 'text'}
      value={source}
      oninput={(event) => edit(event.currentTarget.value)}
      {disabled}
      aria-invalid={Boolean(issue)}
      aria-describedby={`${id}-help`}
      inputmode={schema.type === 'integer' ? 'numeric' : undefined}
    />
  {/if}
  <small id={`${id}-help`}>
    {#if issue}<span role="alert">{issue}</span>
    {:else if schema.description}{schema.description}
    {:else if native}Enter a JSON value. Null, zero, false, empty strings and
      arrays remain distinct.
    {:else if schema.minimum !== undefined}Allowed range: {schema.minimum}–{schema.maximum}.
    {:else}Leave unconfigured to use the profile's ordinary behavior.{/if}
  </small>
  <div class="field-actions">
    {#if native}<button
        class="button button-secondary"
        type="button"
        onclick={setNull}
        {disabled}>Use native null</button
      >{/if}
    <button
      class="button button-secondary"
      type="button"
      onclick={remove}
      disabled={disabled || !present}>Remove value</button
    >
  </div>
</div>

<style>
  .field-heading {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 1rem;
  }
  .presence {
    font-size: var(--text-caption);
    color: var(--foreground-muted);
  }
  textarea {
    width: 100%;
    font-family: var(--font-mono);
    color: var(--foreground);
    background: var(--surface);
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    padding: 0.6rem;
    resize: vertical;
  }
  .field-actions {
    display: flex;
    gap: 0.5rem;
    flex-wrap: wrap;
    margin-top: 0.35rem;
  }
  .field-actions .button {
    min-height: 2rem;
    padding: 0.25rem 0.6rem;
    font-size: var(--text-caption);
  }
  [role='alert'] {
    color: var(--danger);
  }
</style>
