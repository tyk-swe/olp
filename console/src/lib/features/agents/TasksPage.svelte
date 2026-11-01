<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { writable } from 'svelte/store';
  import { errorMessage } from '$lib/api/http';
  import { listProjectMemberships } from '$lib/features/access/projects/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
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

<svelte:head><title>A2A tasks · OpenLLMProxy</title></svelte:head>
<header>
  <p class="eyebrow">Gateway</p>
  <h1>A2A tasks</h1>
  <p>Remote task states and callback counts for your projects</p>
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
{#if tasks.error}
  <p role="alert">{errorMessage(tasks.error)}</p>
{:else if tasks.isFetching}
  <p>Loading tasks…</p>
{:else if !tasks.data?.items.length}
  <p>No tasks in this view</p>
{:else}
  <div class="records">
    <table>
      <thead
        ><tr
          ><th>Task</th><th>Project</th><th>State</th><th>Callbacks</th><th
            >Started</th
          ></tr
        ></thead
      >
      <tbody>
        {#each tasks.data.items as task (task.id)}
          <tr>
            <td
              ><a href={resolve(`/a2a-tasks/${task.id}`)}
                ><code>{task.id}</code></a
              ></td
            >
            <td
              >{projects.data?.find((project) => project.id === task.project_id)
                ?.name ?? task.project_id}</td
            >
            <td>{task.state.replaceAll('_', ' ')}</td>
            <td>{task.callback_attempts ?? 'Unknown'}</td>
            <td>{new Date(task.created_at).toLocaleString()}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}
<nav aria-label="Task pages">
  <button
    type="button"
    disabled={!$filters.cursor || tasks.isFetching}
    onclick={() =>
      filters.update((current) => ({ ...current, cursor: undefined }))}
    >Newest</button
  >
  <button
    type="button"
    disabled={!!tasks.error || !tasks.data?.nextCursor || tasks.isFetching}
    onclick={() =>
      filters.update((current) => ({
        ...current,
        cursor: tasks.data?.nextCursor ?? undefined
      }))}>Older</button
  >
  <button
    type="button"
    disabled={tasks.isFetching}
    onclick={() => tasks.refetch()}>Refresh</button
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
