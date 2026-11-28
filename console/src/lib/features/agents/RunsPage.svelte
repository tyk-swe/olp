<script lang="ts">
  import EmptyState from '#lib/components/EmptyState.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import { emptyCursorHistory, resetCursor } from '#lib/listing/pagination.js';
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import { formatDate } from '#lib/format.js';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { projectQueries } from '#lib/domain/projects/queries.js';
  import { listRuns } from './api';
  import { agentKeys } from './agentKeys';
  import ListPager from './ListPager.svelte';
  import { loadingWhileFetching } from './listQuery';
  import ProjectFilter from './ProjectFilter.svelte';

  let projectID = $state('');
  const pagination = $state(emptyCursorHistory());
  const projects = createQuery(projectQueries.memberships);
  const runs = createQuery(() => ({
    queryKey: agentKeys.runs(projectID, pagination.cursor),
    queryFn: () => listRuns(projectID, pagination.cursor),
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));
</script>

<PageTitle title="Runs" />
<ProjectFilter
  {projects}
  onFilter={(value) => {
    projectID = value;
    resetCursor(pagination);
  }}
/>
<QueryView
  query={loadingWhileFetching(runs)}
  loading="Loading runs…"
  emptyWhen={!runs.data?.items.length}
>
  {#snippet empty()}<EmptyState title="No runs in this view" />{/snippet}
  <ScrollTable label="Agent runs">
    <table class="data-table">
      <thead>
        <tr>
          <th scope="col">Run</th><th scope="col">Project</th><th scope="col">
            API key
          </th><th scope="col">Started</th>
        </tr>
      </thead>
      <tbody>
        {#each runs.data?.items ?? [] as run (run.id)}
          <tr>
            <td>
              <a class="table-link" href={resolve(`activity/runs/${run.id}`)}>
                <code>{run.id}</code>
              </a>
            </td>
            <td>
              {projects.data?.find((project) => project.id === run.project_id)
                ?.name ?? run.project_id}
            </td>
            <td><code>{run.api_key_id}</code></td>
            <td>{formatDate(run.created_at)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </ScrollTable>
</QueryView>
<ListPager {pagination} query={runs} label="Run pages" />
