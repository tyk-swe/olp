<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import { errorMessage } from '#lib/management/http.js';

  let {
    projects,
    onFilter
  }: {
    projects: {
      data?: ReadonlyArray<{ id: string; name: string }>;
      error: unknown;
      isFetching: boolean;
    };
    onFilter: (project: string) => void;
  } = $props();

  let project = $state('');
</script>

<form
  class="filter-bar"
  onsubmit={(event) => {
    event.preventDefault();
    onFilter(project);
  }}
>
  <label class="form-field">
    Project
    <select bind:value={project}>
      <option value="">All authorized projects</option>
      {#each !projects.isFetching && !projects.error ? (projects.data ?? []) : [] as value (value.id)}
        <option value={value.id}>{value.name}</option>
      {/each}
    </select>
  </label>
  <button class="button button-secondary" type="submit">Filter</button>
</form>
{#if projects.error}
  <Callout tone="danger">{errorMessage(projects.error)}</Callout>
{/if}
