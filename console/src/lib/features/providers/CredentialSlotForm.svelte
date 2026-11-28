<script lang="ts">
  import FormField from '#lib/components/FormField.svelte';
  import type { FieldErrors } from '#lib/editing/validation.js';
  import type { SlotField } from './credentialSlotDraft';
  import ExternalReferenceEditor from './ExternalReferenceEditor.svelte';
  import type { CredentialSlot as Slot } from '#lib/domain/providers/api.js';

  let {
    slot,
    grant,
    canManage,
    secret = $bindable(),
    useExternal = $bindable(),
    externalValue = $bindable(),
    models = $bindable(),
    routes = $bindable(),
    keys = $bindable(),
    errors,
    busy,
    onSubmit,
    onCancel
  }: {
    slot: Slot;
    grant: boolean;
    canManage: boolean;
    secret: string;
    useExternal: boolean;
    externalValue: string;
    models: string;
    routes: string;
    keys: string;
    errors: FieldErrors<SlotField>;
    busy: boolean;
    onSubmit: (event: SubmitEvent) => void;
    onCancel: () => void;
  } = $props();
  const limits = [
    ['requests_per_minute', 'Requests / minute'],
    ['tokens_per_minute', 'Tokens / minute'],
    ['input_tokens_per_minute', 'Input tokens / minute'],
    ['output_tokens_per_minute', 'Output tokens / minute'],
    ['max_concurrency', 'Concurrent requests']
  ] as const;
</script>

<form onsubmit={onSubmit}>
  <fieldset disabled={!canManage || Boolean(busy)}>
    <legend class="sr-only">Credential settings</legend>
    <div class="form-grid">
      <FormField label="Name" error={errors.name}>
        {#snippet control(attributes)}
          <input {...attributes} required bind:value={slot.name} />
        {/snippet}
      </FormField>
      <FormField label="Priority" error={errors.priority}>
        {#snippet control(attributes)}
          <input
            {...attributes}
            type="number"
            min="0"
            max="65535"
            bind:value={slot.priority}
          />
        {/snippet}
      </FormField>
      <FormField label="Weight" error={errors.weight}>
        {#snippet control(attributes)}
          <input
            {...attributes}
            type="number"
            min="1"
            bind:value={slot.weight}
          />
        {/snippet}
      </FormField>
      {#if !grant}
        <label>
          <input type="checkbox" bind:checked={useExternal} />Use an external
          credential version
        </label>
        {#if useExternal}<div>
            <ExternalReferenceEditor
              bind:value={externalValue}
              error={errors.reference}
            />
          </div>{:else}<FormField label="Credential" error={errors.credential}>
            {#snippet control(attributes)}
              <input
                {...attributes}
                type="password"
                autocomplete="new-password"
                bind:value={secret}
                placeholder="Leave blank to retain the stored secret"
              />
            {/snippet}
          </FormField>{/if}{/if}
      <FormField label="Allowed models" error={errors.models}>
        {#snippet control(attributes)}
          <input
            {...attributes}
            bind:value={models}
            placeholder="All models, or comma-separated upstream IDs"
          />
        {/snippet}
      </FormField>
      <FormField label="Allowed routes" error={errors.routes}>
        {#snippet control(attributes)}
          <input
            {...attributes}
            bind:value={routes}
            placeholder="All routes, or comma-separated route names"
          />
        {/snippet}
      </FormField>
      <FormField label="Allowed gateway keys" error={errors.keys}>
        {#snippet control(attributes)}
          <input
            {...attributes}
            bind:value={keys}
            placeholder="All keys, or comma-separated key IDs"
          />
        {/snippet}
      </FormField>
      {#each limits as [limit, label] (limit)}
        <FormField {label} error={undefined}>
          {#snippet control(attributes)}
            <input
              {...attributes}
              type="number"
              min="1"
              bind:value={slot[limit]}
            />
          {/snippet}
        </FormField>
      {/each}
      <label>
        <input type="checkbox" bind:checked={slot.enabled} /> Enabled
      </label>
    </div>
    <button class="button button-primary" disabled={Boolean(busy)}>
      Save credential
    </button>
    <button class="button button-secondary" type="button" onclick={onCancel}>
      Cancel
    </button>
  </fieldset>
</form>

<style>
  fieldset {
    border: 0;
    padding: 0;
    margin: 0;
    min-width: 0;
  }
  label {
    display: grid;
    align-content: start;
    gap: 0.35rem;
  }
  fieldset > button {
    margin-top: 1rem;
  }
</style>
