<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import { formatDate } from '#lib/format.js';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { writable } from 'svelte/store';
  import { errorMessage } from '#lib/management/http.js';
  import { projectQueries } from '#lib/domain/projects/queries.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { listInvestigations } from './api';
  import { agentKeys } from './agentKeys';

  const access = useRole();
  const filters = writable<{ project: string; cursor?: string }>({
    project: ''
  });
  const projects = createQuery(projectQueries.memberships);
  const investigations = createQuery(() => ({
    queryKey: agentKeys.investigations($filters.project, $filters.cursor),
    queryFn: () => listInvestigations($filters.project, $filters.cursor),
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));
</script>

<PageTitle title="Investigations" />
<PageHeader
  title="Investigations"
  description="Saved filters, evidence references and operator summaries for your projects."
>
  {#snippet actions()}
    {#if access.allows('POST /api/v1/investigations')}
      <a class="button button-primary" href={resolve('investigations/new')}>
        New investigation
      </a>
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
  <label class="form-field">
    Project
    <select name="project">
      <option value="">All authorized projects</option>
      {#each !projects.isFetching && !projects.error ? (projects.data ?? []) : [] as project (project.id)}
        <option value={project.id}>{project.name}</option>
      {/each}
    </select>
  </label>
  <button class="button button-secondary" type="submit">Filter</button>
</form>
{#if projects.error}<Callout tone="danger">
    {errorMessage(projects.error)}
  </Callout>{/if}
{#if investigations.error}
  <Callout tone="danger">
    {errorMessage(investigations.error)}
    {#snippet actions()}<button
        class="button button-secondary"
        type="button"
        onclick={() => investigations.refetch()}
      >
        Retry
      </button>{/snippet}
  </Callout>
{:else if investigations.isFetching}
  <div class="loading-state" role="status">Loading investigations…</div>
{:else if !investigations.data?.items.length}
  <EmptyState title="No investigations in this view" />
{:else}
  <ScrollTable label="Investigations">
    <table class="data-table">
      <thead>
        <tr>
          <th scope="col">Summary</th><th scope="col">Project</th><th
            scope="col"
          >
            Saved
          </th>
        </tr>
      </thead>
      <tbody>
        {#each investigations.data.items as investigation (investigation.id)}
          <tr>
            <td>
              <a
                class="table-link"
                href={resolve(`investigations/${investigation.id}`)}
              >
                {investigation.summary}
              </a>
            </td>

            <td>
              {projects.data?.find(
                (project) => project.id === investigation.project_id
              )?.name ?? investigation.project_id}
            </td>
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
  >
    Newest
  </button>
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
      }))}
  >
    Older
  </button>
  <button
    class="button button-secondary"
    type="button"
    disabled={investigations.isFetching}
    onclick={() => investigations.refetch()}
  >
    Refresh
  </button>
</nav>
