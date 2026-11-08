<script lang="ts">
  import { formatDate } from '#lib/format.js';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { writable } from 'svelte/store';
  import { errorMessage } from '#lib/management/http.js';
  import { listProjectMemberships } from '#lib/features/access/projects/api.js';
  import { projectKeys } from '#lib/features/access/projects/projectKeys.js';
  import { listRuns } from './api';
  import { agentKeys } from './agentKeys';

  const filters = writable<{ project: string; cursor?: string }>({
    project: ''
  });
  const projects = createQuery(() => ({
    queryKey: projectKeys.memberships,
    queryFn: ({ signal }) => listProjectMemberships(signal),
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));
  const runs = createQuery(() => ({
    queryKey: agentKeys.runs($filters.project, $filters.cursor),
    queryFn: () => listRuns($filters.project, $filters.cursor),
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));
</script>

<svelte:head><title>Runs · OpenLLMProxy</title></svelte:head>
<div class="page-header">
  <div>
    <p class="eyebrow">Agents</p>
    <h1 class="page-title">Runs</h1>
    <p class="page-description">
      Gateway-executed runs for your projects, with steps, approvals and linked
      spans.
    </p>
  </div>
</div>
<form
  class="filter-bar"
  onsubmit={(event) => {
    event.preventDefault();
    const project = new FormData(event.currentTarget).get('project');
    filters.set({ project: typeof project === 'string' ? project : '' });
  }}
>
  <label class="form-field">
    Project
    <select name="project">
      <option value="">All authorized projects</option>
      {#each !projects.isFetching && !projects.error ? (projects.data ?? []) : [] as value (value.id)}
        <option value={value.id}>{value.name}</option>
      {/each}
    </select>
  </label>
  <button class="button button-secondary" type="submit">Filter</button>
</form>
{#if projects.error}
  <p class="inline-problem" role="alert">{errorMessage(projects.error)}</p>
{/if}
{#if runs.error}
  <p class="inline-problem" role="alert">{errorMessage(runs.error)}</p>
{:else if runs.isFetching}
  <div class="loading-state" role="status">Loading runs…</div>
{:else if !runs.data?.items.length}
  <div class="card empty-state">No runs in this view.</div>
{:else}
  <ScrollTable label="Agent runs">
    <table class="data-table">
      <thead
        ><tr
          ><th scope="col">Run</th><th scope="col">Project</th><th scope="col"
            >API key</th
          ><th scope="col">Started</th></tr
        ></thead
      >
      <tbody>
        {#each runs.data.items as run (run.id)}
          <tr>
            <td
              ><a href={resolve(`/runs/${run.id}`)}><code>{run.id}</code></a
              ></td
            >
            <td
              >{projects.data?.find((project) => project.id === run.project_id)
                ?.name ?? run.project_id}</td
            >
            <td><code>{run.api_key_id}</code></td>
            <td>{formatDate(run.created_at)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </ScrollTable>
{/if}
<nav class="page-actions list-pager" aria-label="Run pages">
  <button
    class="button button-secondary"
    type="button"
    disabled={!$filters.cursor || runs.isFetching}
    onclick={() =>
      filters.update((current) => ({ ...current, cursor: undefined }))}
    >Newest</button
  >
  <button
    class="button button-secondary"
    type="button"
    disabled={!!runs.error || !runs.data?.nextCursor || runs.isFetching}
    onclick={() =>
      filters.update((current) => ({
        ...current,
        cursor: runs.data?.nextCursor ?? undefined
      }))}>Older</button
  >
  <button
    class="button button-secondary"
    type="button"
    disabled={runs.isFetching}
    onclick={() => runs.refetch()}>Refresh</button
  >
</nav>
