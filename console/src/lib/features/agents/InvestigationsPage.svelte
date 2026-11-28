<script lang="ts">
  import EmptyState from '#lib/components/EmptyState.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import { emptyCursorHistory, resetCursor } from '#lib/listing/pagination.js';
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import { formatDate } from '#lib/format.js';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { projectQueries } from '#lib/domain/projects/queries.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { listInvestigations } from './api';
  import { agentKeys } from './agentKeys';
  import ListPager from './ListPager.svelte';
  import { loadingWhileFetching } from './listQuery';
  import ProjectFilter from './ProjectFilter.svelte';

  const access = useRole();
  let projectID = $state('');
  const pagination = $state(emptyCursorHistory());
  const projects = createQuery(projectQueries.memberships);
  const investigations = createQuery(() => ({
    queryKey: agentKeys.investigations(projectID, pagination.cursor),
    queryFn: () => listInvestigations(projectID, pagination.cursor),
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
<ProjectFilter
  {projects}
  onFilter={(value) => {
    projectID = value;
    resetCursor(pagination);
  }}
/>
<QueryView
  query={loadingWhileFetching(investigations)}
  loading="Loading investigations…"
  emptyWhen={!investigations.data?.items.length}
>
  {#snippet empty()}<EmptyState
      title="No investigations in this view"
    />{/snippet}
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
        {#each investigations.data?.items ?? [] as investigation (investigation.id)}
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
</QueryView>
<ListPager {pagination} query={investigations} label="Investigation pages" />
