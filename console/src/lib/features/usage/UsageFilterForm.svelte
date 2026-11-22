<script lang="ts">
  import Advanced from '#lib/components/Advanced.svelte';
  import type { UsageDraft } from '#lib/features/usage/usageState.js';

  let {
    draft = $bindable(),
    hasNarrowingFilters,
    onApply,
    onReset
  }: {
    draft: UsageDraft;
    hasNarrowingFilters: boolean;
    onApply: (event: SubmitEvent) => void;
    onReset: () => void;
  } = $props();
</script>

<form class="card filters" aria-label="Usage filters" onsubmit={onApply}>
  <div class="filter-grid">
    <label
      >From <input
        bind:value={draft.start}
        type="datetime-local"
        required
      /></label
    >
    <label
      >To <input bind:value={draft.end} type="datetime-local" required /></label
    >
    <label
      >Break down by <select bind:value={draft.dimension}
        ><option value="route">Route</option><option value="provider"
          >Provider</option
        ><option value="end_user">End user (digest)</option><option
          value="model">Model</option
        ><option value="model_family">Model family</option><option
          value="estimate_provenance">Estimate provenance</option
        ><option value="api_key">API key</option><option value="operation"
          >Operation</option
        ><option value="attribution">Attribution</option><option value="project"
          >Project</option
        ><option value="session">Session</option></select
      ></label
    >
    <label
      >Time buckets <select bind:value={draft.granularity}
        ><option value="hour">Hourly</option><option value="day">Daily</option
        ></select
      ></label
    >
  </div>
  <Advanced label="More filters" open={hasNarrowingFilters}>
    <div class="filter-grid">
      <label
        >Route <input
          bind:value={draft.route}
          placeholder="All routes"
        /></label
      >
      <label
        >Provider ID <input
          bind:value={draft.provider_id}
          class="mono"
          placeholder="All providers"
        /></label
      >
      <label
        >Model <input
          bind:value={draft.model}
          placeholder="All models"
        /></label
      >
      <label
        >API key ID <input
          bind:value={draft.api_key_id}
          class="mono"
          placeholder="All keys"
        /></label
      >
      <label
        >Operation <input
          bind:value={draft.operation}
          placeholder="All operations"
        /></label
      >
      <label
        >Project ID <input
          bind:value={draft.project_id}
          class="mono"
          placeholder="All projects"
        /></label
      >
      <label
        >Session ID <input
          bind:value={draft.session_id}
          class="mono"
          placeholder="All sessions"
        /></label
      >
      <label
        >Attribution key <input
          bind:value={draft.attribution_key}
          placeholder="e.g. team"
        /></label
      >
      <label
        >Attribution value <input
          bind:value={draft.attribution_value}
          placeholder="Any value"
        /></label
      >
    </div>
  </Advanced>
  <div class="filter-actions">
    <button class="button button-primary" type="submit">Apply</button><button
      class="button button-secondary"
      type="button"
      onclick={onReset}>Reset</button
    >
  </div>
</form>

<style>
  .filters {
    margin-top: 1.5rem;
    padding: 1.5rem;
  }
  .filter-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.8rem;
  }
  label {
    display: grid;
    gap: 0.35rem;
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  /* Controls sit inside their caption label and outside .form-field, so the
     caption typography is reset and the shared control recipe restated. */
  input,
  select {
    width: 100%;
    min-height: 2.5rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    transition: border-color var(--motion);
    font-family: var(--font-sans);
    font-size: var(--text-body-sm);
    letter-spacing: normal;
    text-transform: none;
  }
  input.mono {
    font-family: var(--font-mono);
  }
  input:hover,
  select:hover {
    border-color: var(--border-strong);
  }
  input::placeholder {
    color: var(--foreground-muted);
  }
  .filter-actions {
    display: flex;
    gap: 0.65rem;
    margin-top: 1rem;
  }
  @media (max-width: 68rem) {
    .filter-grid {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  @media (max-width: 40rem) {
    .filter-grid {
      grid-template-columns: 1fr;
    }
    .filters {
      padding: 1rem;
    }
  }
</style>
