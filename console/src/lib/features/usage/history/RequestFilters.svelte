<script lang="ts">
  import Advanced from '#lib/components/Advanced.svelte';
  import { timeInputType } from '#lib/listing/filters.js';
  import type { RequestListState } from '#lib/features/usage/history/requestListState.js';

  let {
    listState = $bindable(),
    onApply,
    onReset
  }: {
    listState: RequestListState;
    onApply: (event: SubmitEvent) => void;
    onReset: () => void;
  } = $props();

  const hasMoreFilters = $derived(
    Boolean(
      listState.operation ||
      listState.errorClass ||
      listState.providerId ||
      listState.apiKeyId ||
      listState.projectId ||
      listState.sessionId ||
      listState.attributionKey ||
      listState.attributionValue
    )
  );
</script>

<form class="card filters" aria-label="Request filters" onsubmit={onApply}>
  <div class="filter-grid">
    <label
      >Route <input
        bind:value={listState.route}
        name="route"
        placeholder="support-chat"
      /></label
    >
    <label>Model <input bind:value={listState.model} name="model" /></label>
    <label
      >Status code <input
        bind:value={listState.statusCode}
        name="status"
        inputmode="numeric"
        pattern={'[0-9]{1,5}'}
        placeholder="429"
      /></label
    >
    <label
      >From <input
        bind:value={listState.startedAfter}
        name="after"
        type={timeInputType(listState.startedAfter)}
      /></label
    >
    <label
      >To <input
        bind:value={listState.startedBefore}
        name="before"
        type={timeInputType(listState.startedBefore)}
      /></label
    >
  </div>
  <Advanced label="More filters" open={hasMoreFilters}>
    <div class="filter-grid">
      <label
        >Operation <input
          bind:value={listState.operation}
          name="operation"
          placeholder="generation"
        /></label
      >
      <label
        >Error class <input
          bind:value={listState.errorClass}
          name="error"
        /></label
      >
      <label
        >Provider ID <input
          bind:value={listState.providerId}
          name="provider"
          class="mono"
        /></label
      >
      <label
        >API key ID <input
          bind:value={listState.apiKeyId}
          name="key"
          class="mono"
        /></label
      >
      <label
        >Project ID <input
          bind:value={listState.projectId}
          name="project"
          class="mono"
        /></label
      >
      <label
        >Session ID <input
          bind:value={listState.sessionId}
          name="session"
          class="mono"
        /></label
      >
      <label
        >Attribution key <input
          bind:value={listState.attributionKey}
          name="attribution_key"
        /></label
      >
      <label
        >Attribution value <input
          bind:value={listState.attributionValue}
          name="attribution_value"
        /></label
      >
    </div>
  </Advanced>
  <div class="filter-actions">
    <button class="button button-primary" type="submit">Apply</button>
    <button class="button button-secondary" type="button" onclick={onReset}
      >Reset</button
    >
  </div>
</form>

<style>
  .filters {
    display: grid;
    gap: 1rem;
    padding: 1.25rem;
  }
  .filter-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(10rem, 1fr));
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
  /* Inputs sit inside their caption label and outside .form-field, so the
     caption typography is reset and the shared control recipe restated. */
  input {
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
  input:hover {
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
  @media (max-width: 40rem) {
    .filters {
      padding: 1rem;
    }
  }
</style>
