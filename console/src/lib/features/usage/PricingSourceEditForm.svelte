<script lang="ts">
  import type { PricingSource } from './pricingSources.js';

  let {
    source,
    name = $bindable(),
    url = $bindable(),
    enabled = $bindable(),
    busy,
    onSave,
    onCancel
  }: {
    source: PricingSource;
    name: string;
    url: string;
    enabled: boolean;
    busy: string;
    onSave: () => void;
    onCancel: () => void;
  } = $props();
</script>

<div class="form-grid">
  <div class="form-field">
    <label for={`edit-name-${source.id}`}>Name</label><input
      id={`edit-name-${source.id}`}
      bind:value={name}
      required
    />
  </div>
  <div class="form-field">
    <label for={`edit-url-${source.id}`}>
      Document URL{source.format === 'catalog'
        ? ' (empty for the bundled catalog)'
        : ''}
    </label><input
      id={`edit-url-${source.id}`}
      type="url"
      bind:value={url}
      required={source.format === 'prices'}
    />
  </div>
  <div class="form-field">
    <label for={`edit-enabled-${source.id}`}>Enabled</label><input
      id={`edit-enabled-${source.id}`}
      type="checkbox"
      bind:checked={enabled}
    />
  </div>
</div>
<div class="form-actions">
  <button
    class="text-button"
    type="button"
    disabled={busy === source.id}
    onclick={onSave}
  >
    {busy === source.id ? 'Saving…' : 'Save'}
  </button><button
    class="text-button"
    type="button"
    disabled={Boolean(busy)}
    onclick={onCancel}
  >
    Cancel
  </button>
</div>

<style>
  .form-actions {
    display: flex;
    gap: 0.75rem;
    justify-content: flex-end;
  }
</style>
