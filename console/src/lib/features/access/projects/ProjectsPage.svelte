<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import Dialog from '#lib/components/Dialog.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import StatusBadge from '#lib/components/StatusBadge.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { useMutation } from '#lib/editing/action.svelte.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { errorMessage } from '#lib/management/http.js';
  import { createProject } from './api';
  import { projectKeys } from '#lib/domain/projects/projectKeys.js';
  import { projectQueries } from '#lib/domain/projects/queries.js';

  const access = useRole();
  const queryClient = useQueryClient();
  const creation = useMutation();
  const projects = createQuery(projectQueries.memberships);
  const canCreate = $derived(access.allows('POST /api/v1/projects'));

  let creating = $state(false);
  let name = $state('');

  function startCreate() {
    name = '';
    creation.error = '';
    creating = true;
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!name.trim()) return;
    await creation.submit(name, createProject, async (project) => {
      await queryClient.invalidateQueries({ queryKey: projectKeys.root });
      creating = false;
      notify(`Created ${project.name}`);
      await goto(resolve(`projects/${project.id}`));
    });
  }
</script>

<PageTitle title="Projects" />

<PageHeader
  title="Projects"
  description="Projects group API keys, routes and limits for a team or app"
>
  {#snippet actions()}{#if canCreate}<button
        class="button button-primary"
        type="button"
        onclick={startCreate}
      >
        New project
      </button>{/if}{/snippet}
</PageHeader>

<QueryView
  query={projects}
  loading="Loading projects…"
  errorTitle="Projects didn’t load"
  failure={errorMessage(projects.error)}
  retryLabel="Try again"
  emptyWhen={!projects.data?.length}
>
  {#snippet empty()}
    <EmptyState title="No projects yet">
      {canCreate
        ? 'Create a project to give a team its own keys, routes and limits.'
        : 'Ask an owner to add you to a project.'}
      {#snippet action()}{#if canCreate}<button
            class="button button-primary"
            type="button"
            onclick={startCreate}
          >
            New project
          </button>{/if}{/snippet}
    </EmptyState>
  {/snippet}
  <ScrollTable label="Projects">
    <table class="data-table">
      <thead><tr><th>Name</th><th>Your role</th></tr></thead>
      <tbody>
        {#each projects.data ?? [] as project (project.id)}
          <tr>
            <td>
              <a class="table-link" href={resolve(`projects/${project.id}`)}>
                <strong>{project.name}</strong>
              </a>
            </td>
            <td>
              <StatusBadge
                label={project.role === 'manager' ? 'Manager' : 'Viewer'}
                tone={project.role === 'manager' ? 'accent' : 'neutral'}
              />
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </ScrollTable>
</QueryView>

{#if creating}
  <Dialog title="New project" onClose={() => (creating = false)}>
    {#snippet children(close)}
      <form class="dialog-form" onsubmit={submit}>
        <div class="form-field">
          <label for="new-project-name">Name</label>
          <input
            id="new-project-name"
            data-autofocus
            bind:value={name}
            maxlength="100"
            required
            disabled={creation.busy}
          />
        </div>
        {#if creation.error}<p class="inline-problem" role="alert">
            {creation.error}
          </p>{/if}
        <div class="page-actions">
          <button class="button button-secondary" type="button" onclick={close}>
            Cancel
          </button>
          <button
            class="button button-primary"
            type="submit"
            disabled={creation.busy || !name.trim()}
          >
            {creation.busy ? 'Creating…' : 'Create project'}
          </button>
        </div>
      </form>
    {/snippet}
  </Dialog>
{/if}

<style>
  .dialog-form {
    display: grid;
    gap: 1rem;
  }
  .dialog-form .page-actions {
    justify-content: flex-end;
  }
</style>
