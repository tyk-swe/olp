<script lang="ts">
  import EmptyState from '#lib/components/EmptyState.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import { emptyCursorHistory, resetCursor } from '#lib/listing/pagination.js';
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import { formatDate, stateLabel } from '#lib/format.js';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { projectQueries } from '#lib/domain/projects/queries.js';
  import { listTasks } from './api';
  import { agentKeys } from './agentKeys';
  import ListPager from './ListPager.svelte';
  import { loadingWhileFetching } from './listQuery';
  import ProjectFilter from './ProjectFilter.svelte';

  let projectID = $state('');
  const pagination = $state(emptyCursorHistory());
  const projects = createQuery(projectQueries.memberships);
  const tasks = createQuery(() => ({
    queryKey: agentKeys.tasks(projectID, pagination.cursor),
    queryFn: () => listTasks(projectID, pagination.cursor),
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));
</script>

<PageTitle title="A2A tasks" />
<ProjectFilter
  {projects}
  onFilter={(value) => {
    projectID = value;
    resetCursor(pagination);
  }}
/>
<QueryView
  query={loadingWhileFetching(tasks)}
  loading="Loading tasks…"
  emptyWhen={!tasks.data?.items.length}
>
  {#snippet empty()}<EmptyState title="No tasks in this view" />{/snippet}
  <ScrollTable label="A2A tasks">
    <table class="data-table">
      <thead>
        <tr>
          <th scope="col">Task</th><th scope="col">Project</th><th scope="col">
            State
          </th><th scope="col">Callbacks</th><th scope="col">Started</th>
        </tr>
      </thead>
      <tbody>
        {#each tasks.data?.items ?? [] as task (task.id)}
          <tr>
            <td>
              <a class="table-link" href={resolve(`activity/tasks/${task.id}`)}>
                <code>{task.id}</code>
              </a>
            </td>
            <td>
              {projects.data?.find((project) => project.id === task.project_id)
                ?.name ?? task.project_id}
            </td>
            <td>{stateLabel(task.state)}</td>
            <td>{task.callback_attempts ?? 'Unknown'}</td>
            <td>{formatDate(task.created_at)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </ScrollTable>
</QueryView>
<ListPager {pagination} query={tasks} label="Task pages" />
