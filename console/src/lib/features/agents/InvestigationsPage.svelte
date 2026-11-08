<script lang="ts">
  import PageHeader from '#lib/components/PageHeader.svelte';
  import { formatDate } from '#lib/format.js';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { writable } from 'svelte/store';
  import { errorMessage } from '#lib/management/http.js';
  import { listProjectMemberships } from '#lib/features/access/projects/api.js';
  import { projectKeys } from '#lib/features/access/projects/projectKeys.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
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
<PageHeader
  title="Investigations"
  description="Saved filters, evidence references and operator summaries for your projects."
>
  {#snippet actions()}
    {#if access.allows('POST /api/v1/investigations')}
      <a class="button button-primary" href={resolve('investigations/new')}
        >New investigation</a
      >
    {/if}
  {/snippet}
</PageHeader>
<form
  class="filter-bar"
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
{#if projects.error}<p class="inline-problem" role="alert">
    {errorMessage(projects.error)}
  </p>{/if}
{#if investigations.error}
  <p class="inline-problem" role="alert">
    {errorMessage(investigations.error)}
  </p>
{:else if investigations.isFetching}
  <div class="loading-state" role="status">Loading investigations…</div>
{:else if !investigations.data?.items.length}
  <div class="card empty-state">No investigations in this view.</div>
{:else}
  <ScrollTable label="Investigations">
    <table class="data-table">
      <thead
        ><tr
          ><th scope="col">Summary</th><th scope="col">Project</th><th
            scope="col">Saved</th
          ></tr
        ></thead
      >
      <tbody>
        {#each investigations.data.items as investigation (investigation.id)}
          <tr>
            <td
              ><a
                class="text-button"
                href={resolve(`investigations/${investigation.id}`)}
                >{investigation.summary}</a
              >
            </td>

            <td
              >{projects.data?.find(
                (project) => project.id === investigation.project_id
              )?.name ?? investigation.project_id}</td
            >
            <td>{formatDate(investigation.created_at)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </ScrollTable>
{/if}
<nav class="page-actions list-pager" aria-label="Investigation pages">
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
