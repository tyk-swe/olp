<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import { formatDate, stateLabel } from '#lib/format.js';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { writable } from 'svelte/store';
  import { errorMessage } from '#lib/management/http.js';
  import {
    listProjectMemberships,
    projectKeys
  } from '#lib/features/access/index.js';
  import { listTasks } from './api';
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
  const tasks = createQuery(() => ({
    queryKey: agentKeys.tasks($filters.project, $filters.cursor),
    queryFn: () => listTasks($filters.project, $filters.cursor),
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));
</script>

<PageTitle title="A2A tasks" />
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
  <Callout tone="danger">{errorMessage(projects.error)}</Callout>
{/if}
{#if tasks.error}
  <Callout tone="danger">
    {errorMessage(tasks.error)}
    {#snippet actions()}<button
        class="button button-secondary"
        type="button"
        onclick={() => tasks.refetch()}
      >
        Retry
      </button>{/snippet}
  </Callout>
{:else if tasks.isFetching}
  <div class="loading-state" role="status">Loading tasks…</div>
{:else if !tasks.data?.items.length}
  <EmptyState title="No tasks in this view" />
{:else}
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
        {#each tasks.data.items as task (task.id)}
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
{/if}
<nav class="page-actions list-pager" aria-label="Task pages">
  <button
    class="button button-secondary"
    type="button"
    disabled={!$filters.cursor || tasks.isFetching}
    onclick={() =>
      filters.update((current) => ({ ...current, cursor: undefined }))}
  >
    Newest
  </button>
  <button
    class="button button-secondary"
    type="button"
    disabled={!!tasks.error || !tasks.data?.nextCursor || tasks.isFetching}
    onclick={() =>
      filters.update((current) => ({
        ...current,
        cursor: tasks.data?.nextCursor ?? undefined
      }))}
  >
    Older
  </button>
  <button
    class="button button-secondary"
    type="button"
    disabled={tasks.isFetching}
    onclick={() => tasks.refetch()}
  >
    Refresh
  </button>
</nav>
