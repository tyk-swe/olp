<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { writable } from 'svelte/store';
  import { errorMessage } from '$lib/api/http';
  import { listProjectMemberships } from '$lib/features/access/projects/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
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
<header>
  <p class="eyebrow">Gateway</p>
  <h1>Runs</h1>
  <p>
    Gateway-executed runs for your projects, with steps, approvals and linked
    spans.
  </p>
</header>
<form
  onsubmit={(event) => {
    event.preventDefault();
    const project = new FormData(event.currentTarget).get('project');
    filters.set({ project: typeof project === 'string' ? project : '' });
  }}
>
  <label>
    Project
    <select name="project">
      <option value="">All authorized projects</option>
      {#each !projects.isFetching && !projects.error ? (projects.data ?? []) : [] as value (value.id)}
        <option value={value.id}>{value.name}</option>
      {/each}
    </select>
  </label>
  <button type="submit">Filter</button>
</form>
{#if projects.error}
  <p role="alert">{errorMessage(projects.error)}</p>
{/if}
{#if runs.error}
  <p role="alert">{errorMessage(runs.error)}</p>
{:else if runs.isFetching}
  <p>Loading runs…</p>
{:else if !runs.data?.items.length}
  <p>No runs in this view.</p>
{:else}
  <div class="records">
    <table>
      <thead
        ><tr><th>Run</th><th>Project</th><th>API key</th><th>Started</th></tr
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
            <td>{new Date(run.created_at).toLocaleString()}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}
<nav aria-label="Run pages">
  <button
    type="button"
    disabled={!$filters.cursor || runs.isFetching}
    onclick={() =>
      filters.update((current) => ({ ...current, cursor: undefined }))}
    >Newest</button
  >
  <button
    type="button"
    disabled={!!runs.error || !runs.data?.nextCursor || runs.isFetching}
    onclick={() =>
      filters.update((current) => ({
        ...current,
        cursor: runs.data?.nextCursor ?? undefined
      }))}>Older</button
  >
  <button
    type="button"
    disabled={runs.isFetching}
    onclick={() => runs.refetch()}>Refresh</button
  >
</nav>

<style>
  header,
  form {
    margin-bottom: 1rem;
  }
  form {
    display: flex;
    align-items: end;
    flex-wrap: wrap;
    gap: 1rem;
  }
  label {
    display: grid;
    gap: 0.25rem;
  }
  .records {
    overflow-x: auto;
  }
  table {
    width: 100%;
    text-align: left;
  }
  td,
  th {
    padding: 0.75rem;
    vertical-align: top;
  }
  code {
    overflow-wrap: anywhere;
  }
  nav {
    display: flex;
    gap: 0.5rem;
    margin-top: 1rem;
  }
</style>
