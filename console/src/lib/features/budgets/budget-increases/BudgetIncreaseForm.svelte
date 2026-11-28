<script lang="ts">
  import EntityPicker from '#lib/components/EntityPicker.svelte';
  import FormField from '#lib/components/FormField.svelte';
  import type { EntitySource } from '#lib/components/entityOptions.js';
  import type { FieldErrors } from '#lib/editing/validation.js';
  import type { IncreaseInput } from './api';
  import type { IncreaseDraft, IncreaseField } from './increaseDraft';

  let {
    draft = $bindable(),
    errors,
    busy,
    submitted,
    canOrganize,
    canInstall,
    targetPicker,
    kinds,
    onSubmit,
    onReset
  }: {
    draft: IncreaseDraft;
    errors: FieldErrors<IncreaseField>;
    busy: boolean;
    submitted: IncreaseInput | null;
    canOrganize: boolean;
    canInstall: boolean;
    targetPicker: { source: EntitySource; label: string } | null;
    kinds: { value: IncreaseInput['target']['kind']; label: string }[];
    onSubmit: (event: SubmitEvent & { currentTarget: HTMLFormElement }) => void;
    onReset: () => void;
  } = $props();
</script>

<form onsubmit={onSubmit}>
  <fieldset disabled={busy || submitted !== null}>
    <div class="fields">
      <FormField label="Budget boundary" error={errors.kind}>
        {#snippet control(attributes)}
          <select {...attributes} bind:value={draft.kind}>
            {#each kinds as option (option.value)}<option value={option.value}>
                {option.label}
              </option>{/each}{#if canOrganize}<option value="organization">
                Organization
              </option>{/if}{#if canInstall}<option value="installation">
                Installation
              </option>{/if}
          </select>
        {/snippet}
      </FormField>
      {#if targetPicker}<FormField
          label={targetPicker.label}
          error={errors.resourceId}
        >
          {#snippet control(attributes)}
            {#key targetPicker.source}<EntityPicker
                {...attributes}
                source={targetPicker.source}
                bind:value={draft.resourceId}
                emptyLabel="Choose one"
                required
              />{/key}
          {/snippet}
        </FormField>{:else if draft.kind !== 'installation'}<FormField
          label="API key ID"
          error={errors.resourceId}
        >
          {#snippet control(attributes)}
            <input
              {...attributes}
              class="mono"
              bind:value={draft.resourceId}
              required
            />
          {/snippet}
        </FormField>{/if}
      {#if draft.kind === 'key_route'}<FormField
          label="Route slug"
          error={errors.route}
        >
          {#snippet control(attributes)}
            <input {...attributes} bind:value={draft.route} required />
          {/snippet}
        </FormField>{/if}
      {#if draft.kind === 'key_end_user' || draft.kind === 'project_end_user'}<FormField
          label="End-user digest"
          error={errors.digest}
        >
          {#snippet control(attributes)}
            <input
              {...attributes}
              bind:value={draft.digest}
              required
              pattern={'[0-9a-f]{64}'}
            /><small>
              Use the digest from end-user lookup, never a raw identifier.
            </small>
          {/snippet}
        </FormField>{/if}
      {#if draft.kind === 'attribution'}<div class="form-field">
          <label for="increase-label">Attribution label</label><input
            id="increase-label"
            bind:value={draft.label}
            required
          />
        </div>
        <div class="form-field">
          <label for="increase-value">Attribution value</label><input
            id="increase-value"
            bind:value={draft.value}
            required
          />
        </div>{/if}
      <FormField label="Budget window" error={errors.window}>
        {#snippet control(attributes)}
          <select {...attributes} bind:value={draft.window}>
            <option value="day">Day</option><option value="week">
              Week
            </option><option value="month">Month</option>
          </select>
        {/snippet}
      </FormField>
      <FormField label="Additional USD" error={errors.amount}>
        {#snippet control(attributes)}
          <input
            {...attributes}
            bind:value={draft.amount}
            inputmode="decimal"
            required
          />
        {/snippet}
      </FormField>
      <FormField label="Earlier expiry (optional)" error={errors.expires}>
        {#snippet control(attributes)}
          <input
            {...attributes}
            type="datetime-local"
            bind:value={draft.expires}
          /><small>Your local time. Blank uses the budget-window end.</small>
        {/snippet}
      </FormField>
      <FormField label="Reason" error={errors.reason}>
        {#snippet control(attributes)}
          <input
            {...attributes}
            bind:value={draft.reason}
            required
            maxlength="256"
          />
        {/snippet}
      </FormField>
    </div>
  </fieldset>
  <button class="button button-primary" disabled={busy} type="submit">
    {submitted ? 'Retry increase' : 'Create increase'}
  </button>
  {#if submitted}<button
      class="button button-secondary"
      type="button"
      disabled={busy}
      onclick={onReset}
    >
      Start another request
    </button>
    <p>
      Retry preserves the original request to recover a lost response without
      adding another increase.
    </p>{/if}
</form>

<style>
  .fields {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 18rem), 1fr));
    gap: 1rem;
  }
  fieldset {
    border: 0;
    padding: 0;
    margin: 1rem 0;
  }
</style>
