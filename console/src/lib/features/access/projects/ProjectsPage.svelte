<script lang="ts">
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import StatusBadge from '#lib/components/StatusBadge.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { useMutationIntent } from '#lib/editing/mutationIntent.svelte.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import { errorMessage } from '#lib/management/http.js';
  import { createProject, listProjectMemberships } from './api';
  import { projectKeys } from './projectKeys';

  const access = useRole();
  const queryClient = useQueryClient();
  const createIntent = useMutationIntent();
  const projects = createQuery(() => ({
    queryKey: projectKeys.memberships,
    queryFn: ({ signal }) => listProjectMemberships(signal)
  }));
  const canCreate = $derived(access.allows('POST /api/v1/projects'));

  let creating = $state(false);
  let name = $state('');
  let busy = $state(false);
  let problem = $state('');

  function startCreate() {
    name = '';
    problem = '';
    creating = true;
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (busy || !name.trim()) return;
    busy = true;
    problem = '';
    try {
      const submission =
        createIntent.prepare<Parameters<typeof createProject>[0]>(name);
      const project = await createProject(
        submission.input,
        submission.requestId
      );
      createIntent.reset();
      await queryClient.invalidateQueries({ queryKey: projectKeys.root });
      creating = false;
      notify(`Created ${project.name}`);
      await goto(resolve(`projects/${project.id}`));
    } catch (error) {
      problem = errorMessage(error);
    } finally {
      busy = false;
    }
  }
</script>

<svelte:head><title>Projects · OpenLLMProxy</title></svelte:head>

<PageHeader
  title="Projects"
  description="Projects group API keys, routes and limits for a team or app"
>
  {#snippet actions()}{#if canCreate}<button
        class="button button-primary"
        type="button"
        onclick={startCreate}>New project</button
      >{/if}{/snippet}
</PageHeader>

{#if projects.isPending}
  <div class="loading-state" role="status">Loading projects…</div>
{:else if projects.isError}
  <Callout tone="danger" title="Projects didn’t load">
    {errorMessage(projects.error)}
    {#snippet actions()}<button
        class="button button-secondary"
        type="button"
        onclick={() => projects.refetch()}>Try again</button
      >{/snippet}
  </Callout>
{:else if !projects.data.length}
  <EmptyState title="No projects yet">
    {canCreate
      ? 'Create a project to give a team its own keys, routes and limits.'
      : 'Ask an owner to add you to a project.'}
    {#snippet action()}{#if canCreate}<button
          class="button button-primary"
          type="button"
          onclick={startCreate}>New project</button
        >{/if}{/snippet}
  </EmptyState>
{:else}
  <div class="table-shell">
    <table class="data-table">
      <thead><tr><th>Name</th><th>Your role</th></tr></thead>
      <tbody>
        {#each projects.data as project (project.id)}
          <tr>
            <td
              ><a href={resolve(`projects/${project.id}`)}
                ><strong>{project.name}</strong></a
              ></td
            >
            <td
              ><StatusBadge
                label={project.role === 'manager' ? 'Manager' : 'Viewer'}
                tone={project.role === 'manager' ? 'accent' : 'neutral'}
              /></td
            >
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
{/if}

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
            disabled={busy}
          />
        </div>
        {#if problem}<p class="inline-problem" role="alert">{problem}</p>{/if}
        <div class="page-actions">
          <button class="button button-secondary" type="button" onclick={close}
            >Cancel</button
          >
          <button
            class="button button-primary"
            type="submit"
            disabled={busy || !name.trim()}
            >{busy ? 'Creating…' : 'Create project'}</button
          >
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
