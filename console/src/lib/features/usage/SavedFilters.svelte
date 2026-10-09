<script lang="ts">
  import { onMount } from 'svelte';
  import { authLifecycle } from '$lib/features/access/session/lifecycle';
  import { errorMessage } from '$lib/api/http';
  import {
    readSavedFilters,
    saveFilters,
    type FilterScope,
    type SavedFilter
  } from './savedFilters';
  let {
    scope,
    search,
    apply
  }: { scope: FilterScope; search: string; apply: (search: string) => void } =
    $props();
  let session = $state('');
  let filters = $state<SavedFilter[]>([]);
  let name = $state('');
  let selected = $state('');
  let error = $state('');
  onMount(() =>
    authLifecycle.subscribe((snapshot) => {
      const next =
        snapshot.phase === 'authenticated' ? snapshot.sessionId || '' : '';
      if (next === session) return;
      session = next;
      selected = '';
      filters = [];
      if (session) {
        try {
          filters = readSavedFilters(window.sessionStorage, session, scope);
        } catch {
          error = 'Saved views are unavailable in this browser.';
        }
      }
    })
  );

  function save(event: SubmitEvent) {
    event.preventDefault();
    try {
      const label = name.trim();
      const next = [
        ...filters.filter((entry) => entry.name !== label),
        { name: label, search }
      ];
      saveFilters(window.sessionStorage, session, scope, next);
      filters = next;
      selected = label;
      name = error = '';
    } catch (cause) {
      error = errorMessage(cause);
    }
  }

  function remove() {
    try {
      const next = filters.filter((entry) => entry.name !== selected);
      saveFilters(window.sessionStorage, session, scope, next);
      filters = next;
      selected = error = '';
    } catch (cause) {
      error = errorMessage(cause);
    }
  }
</script>

<details class="saved-views">
  <summary>Saved views</summary>
  <p class="helper">
    Save the current filters for this sign-in session and browser tab. A new
    session starts with no saved views.
  </p>
  <div class="view-actions">
    <label
      >Saved view<select
        class="filter-control"
        aria-label="Saved view"
        bind:value={selected}
        ><option value="">Choose a view</option
        >{#each filters as filter (filter.name)}<option value={filter.name}
            >{filter.name}</option
          >{/each}</select
      ></label
    >
    <button
      class="button button-secondary"
      type="button"
      disabled={!selected || !session}
      onclick={() => {
        const view = filters.find((entry) => entry.name === selected);
        if (view) apply(view.search);
      }}>Apply view</button
    >
    <button
      class="button button-quiet"
      type="button"
      disabled={!selected || !session}
      onclick={remove}>Delete view</button
    >
  </div>
  <form onsubmit={save}>
    <label
      >View name<input
        class="filter-control"
        bind:value={name}
        maxlength="60"
        required
        disabled={!session}
      /></label
    ><button class="button button-secondary" disabled={!session}
      >Save current filters</button
    >
  </form>
  {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
</details>

<style>
  .saved-views {
    margin-block: 1rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-card);
    padding: 1rem;
  }
  summary {
    cursor: pointer;
    color: var(--foreground);
  }
  .view-actions,
  form {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
    align-items: end;
    margin-top: 1rem;
  }
  label {
    display: grid;
    gap: 0.4rem;
    min-width: 12rem;
  }
</style>
