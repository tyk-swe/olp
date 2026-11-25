<script lang="ts">
  import ExternalReferenceEditor from './ExternalReferenceEditor.svelte';
  import type { CredentialSlot as Slot } from './credentials.js';

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
    busy: boolean;
    onSubmit: (event: SubmitEvent) => void;
    onCancel: () => void;
  } = $props();
</script>

<form onsubmit={onSubmit}>
  <fieldset disabled={!canManage || Boolean(busy)}>
    <legend class="sr-only">Credential settings</legend>
    <div class="form-grid">
      <label>Name<input required bind:value={slot.name} /></label>
      <label>
        Priority<input
          type="number"
          min="0"
          max="65535"
          bind:value={slot.priority}
        />
      </label>
      <label>
        Weight<input type="number" min="1" bind:value={slot.weight} />
      </label>
      {#if !grant}
        <label>
          <input type="checkbox" bind:checked={useExternal} />Use an external
          credential version
        </label>
        {#if useExternal}<ExternalReferenceEditor
            bind:value={externalValue}
          />{:else}<label>
            Credential<input
              type="password"
              autocomplete="new-password"
              bind:value={secret}
              placeholder="Leave blank to retain the stored secret"
            />
          </label>{/if}{/if}
      <label>
        Allowed models<input
          bind:value={models}
          placeholder="All models, or comma-separated upstream IDs"
        />
      </label>
      <label>
        Allowed routes<input
          bind:value={routes}
          placeholder="All routes, or comma-separated route names"
        />
      </label>
      <label>
        Allowed gateway keys<input
          bind:value={keys}
          placeholder="All keys, or comma-separated key IDs"
        />
      </label>
      <label>
        Requests / minute<input
          type="number"
          min="1"
          bind:value={slot.requests_per_minute}
        />
      </label>
      <label>
        Tokens / minute<input
          type="number"
          min="1"
          bind:value={slot.tokens_per_minute}
        />
      </label>
      <label>
        Input tokens / minute<input
          type="number"
          min="1"
          bind:value={slot.input_tokens_per_minute}
        />
      </label>
      <label>
        Output tokens / minute<input
          type="number"
          min="1"
          bind:value={slot.output_tokens_per_minute}
        />
      </label>
      <label>
        Concurrent requests<input
          type="number"
          min="1"
          bind:value={slot.max_concurrency}
        />
      </label>
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
    gap: 0.35rem;
  }
  fieldset > button {
    margin-top: 1rem;
  }

  fieldset {
    border: 0;
    padding: 0;
    margin: 0;
    min-width: 0;
  }
  label {
    display: grid;
    gap: 0.35rem;
  }
  fieldset > button {
    margin-top: 1rem;
  }
</style>
