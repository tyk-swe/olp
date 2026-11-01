<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { writable } from 'svelte/store';
  import { errorMessage } from '$lib/api/http';
  import { listProjectMemberships } from '$lib/features/access/projects/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { listInvestigations } from './api';
  import { agentKeys } from './agentKeys';

  const access = useRole();
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
  const investigations = createQuery(() => ({
    queryKey: agentKeys.investigations($filters.project, $filters.cursor),
    queryFn: () => listInvestigations($filters.project, $filters.cursor),
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));
</script>

<svelte:head><title>Investigations · OpenLLMProxy</title></svelte:head>
<header>
  <p class="eyebrow">Gateway</p>
  <h1>Investigations</h1>
  <p>
    Saved filters, evidence references and operator summaries for your projects.
  </p>
  {#if access.allows('POST /api/v1/investigations')}
    <a class="button button-primary" href={resolve('/investigations/new')}
      >New investigation</a
    >
  {/if}
</header>
<form
  onsubmit={(event) => {
    event.preventDefault();
    const project = new FormData(event.currentTarget).get('project');
    filters.set({ project: typeof project === 'string' ? project : '' });
  }}
>
  <label class="form-field"
    >Project
    <select name="project">
      <option value="">All authorized projects</option>
      {#each !projects.isFetching && !projects.error ? (projects.data ?? []) : [] as project (project.id)}
        <option value={project.id}>{project.name}</option>
      {/each}
    </select>
  </label>
  <button class="button button-secondary" type="submit">Filter</button>
</form>
{#if projects.error}<p role="alert">{errorMessage(projects.error)}</p>{/if}
{#if investigations.error}
  <p role="alert">{errorMessage(investigations.error)}</p>
{:else if investigations.isFetching}
  <p>Loading investigations…</p>
{:else if !investigations.data?.items.length}
  <p>No investigations in this view.</p>
{:else}
  <div class="records">
    <table>
      <thead><tr><th>Summary</th><th>Project</th><th>Saved</th></tr></thead>
      <tbody>
        {#each investigations.data.items as investigation (investigation.id)}
          <tr>
            <td
              ><a
                class="text-button"
                href={resolve(`/investigations/${investigation.id}`)}
                >{investigation.summary}</a
              ></td
            >
            <td
              >{projects.data?.find(
                (project) => project.id === investigation.project_id
              )?.name ?? investigation.project_id}</td
            >
            <td>{new Date(investigation.created_at).toLocaleString()}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}
<nav aria-label="Investigation pages">
  <button
    class="button button-secondary"
    type="button"
    disabled={!$filters.cursor || investigations.isFetching}
    onclick={() =>
      filters.update((current) => ({ ...current, cursor: undefined }))}
    >Newest</button
  >
  <button
    class="button button-secondary"
    type="button"
    disabled={!!investigations.error ||
      !investigations.data?.nextCursor ||
      investigations.isFetching}
    onclick={() =>
      filters.update((current) => ({
        ...current,
        cursor: investigations.data?.nextCursor ?? undefined
      }))}>Older</button
  >
  <button
    class="button button-secondary"
    type="button"
    disabled={investigations.isFetching}
    onclick={() => investigations.refetch()}>Refresh</button
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
  nav {
    display: flex;
    gap: 0.5rem;
    margin-top: 1rem;
  }
</style>
