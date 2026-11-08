<script lang="ts">
  import { resolve } from '$app/paths';
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import StatusBadge from '#lib/components/StatusBadge.svelte';
  import PublicationPanel from '#lib/features/model-catalog/PublicationPanel.svelte';
  import AggregateBudgetPanel from '../budgets/AggregateBudgetPanel.svelte';
  import ProjectLimitTemplatesPanel from './ProjectLimitTemplatesPanel.svelte';
  import ProjectAttributionBudgetsPanel from './ProjectAttributionBudgetsPanel.svelte';
  import ProjectRouteGroupsPanel from './ProjectRouteGroupsPanel.svelte';
  import ProjectAttributionPolicyPanel from './ProjectAttributionPolicyPanel.svelte';
  import { goto } from '$app/navigation';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import Dialog from '#lib/components/Dialog.svelte';
  import Menu from '#lib/components/Menu.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { errorMessage, isEtagMismatch } from '#lib/management/http.js';
  import { useRole } from '../session/useRole.svelte';
  import {
    deleteProject,
    getProject,
    listProjectMemberships,
    renameProject
  } from './api';
  import ProjectMembers from './ProjectMembers.svelte';
  import { projectKeys } from './projectKeys';
  import ProjectEndUserPolicyPanel from './ProjectEndUserPolicyPanel.svelte';

  const access = useRole();
  const projects = createQuery(() => ({
    queryKey: projectKeys.memberships,
    queryFn: ({ signal }) => listProjectMemberships(signal)
  }));
  let { projectId }: { projectId: string } = $props();
  const selected = $derived(
    projects.data?.find((project) => project.id === projectId)
  );
  const queryClient = useQueryClient();
  const canManage = $derived(
    access.allows('GET /api/v1/projects/{project_id}')
  );
  const detail = createQuery(() => ({
    queryKey: projectKeys.detail(projectId),
    enabled: canManage,
    queryFn: ({ signal }) => getProject(projectId, signal)
  }));

  let renaming = $state(false);
  let renameValue = $state('');
  let renameBusy = $state(false);
  let actionProblem = $state('');

  const conflict = (error: unknown) =>
    isEtagMismatch(error)
      ? 'Someone else changed this project. Reload the page and try again.'
      : errorMessage(error);

  async function submitRename(event: SubmitEvent) {
    event.preventDefault();
    if (!detail.data || renameBusy || !renameValue.trim()) return;
    renameBusy = true;
    actionProblem = '';
    try {
      await renameProject(detail.data, renameValue.trim());
      await queryClient.invalidateQueries({ queryKey: projectKeys.root });
      renaming = false;
      notify('Project renamed');
    } catch (error) {
      actionProblem = conflict(error);
    } finally {
      renameBusy = false;
    }
  }

  async function remove() {
    const project = detail.data;
    if (!project) return;
    const confirmed = await confirmAction({
      title: `Delete ${project.name}?`,
      body: 'Only empty projects can be deleted. Move or delete its keys, routes and providers first.',
      confirmLabel: 'Delete project',
      danger: true
    });
    if (!confirmed) return;
    actionProblem = '';
    try {
      await deleteProject(project, crypto.randomUUID());
      await queryClient.invalidateQueries({ queryKey: projectKeys.root });
      notify(`Deleted ${project.name}`);
      await goto(resolve('projects'));
    } catch (error) {
      actionProblem = conflict(error);
    }
  }

  const editable = $derived(
    access.allows('PUT /api/v1/projects/{project_id}/end-user-policy') &&
      (access.globalScope || selected?.role === 'manager')
  );
</script>

<svelte:head
  ><title>{selected?.name ?? 'Project'} · OpenLLMProxy</title></svelte:head
>

{#if projects.isPending}
  <div class="loading-state" role="status">Loading project…</div>
{:else if projects.isError}
  <Callout tone="danger" title="Project didn’t load">
    {errorMessage(projects.error)}
    {#snippet actions()}<button
        class="button button-secondary"
        type="button"
        onclick={() => projects.refetch()}>Try again</button
      >{/snippet}
  </Callout>
{:else if !selected}
  <EmptyState title="Project not found">
    It may have been removed, or you may not be a member.
    {#snippet action()}<a
        class="button button-secondary"
        href={resolve('projects')}>All projects</a
      >{/snippet}
  </EmptyState>
{:else}
  <PageHeader title={selected.name}>
    {#snippet actions()}{#if detail.data}<Menu label="Project actions">
          {#if access.allows('PATCH /api/v1/projects/{project_id}')}<button
              type="button"
              onclick={() => {
                renameValue = selected.name;
                actionProblem = '';
                renaming = true;
              }}>Rename</button
            >{/if}
          {#if access.allows('DELETE /api/v1/projects/{project_id}')}<button
              type="button"
              class="danger"
              onclick={remove}>Delete project</button
            >{/if}
        </Menu>{/if}{/snippet}
    {#snippet meta()}<StatusBadge
        label={selected.role === 'manager' ? 'Manager' : 'Member'}
        tone="neutral"
      />{/snippet}
  </PageHeader>
  {#if !editable}
    <p class="read-only-note">
      You can see these settings. Changing them needs a project manager who can
      manage keys.
    </p>
  {/if}
  {#if actionProblem && !renaming}<Callout tone="danger"
      >{actionProblem}</Callout
    >{/if}
  {#key selected.id}
    {#if detail.data}
      <section class="group" aria-labelledby="project-members">
        <h2 id="project-members">Members</h2>
        <ProjectMembers project={detail.data} />
      </section>
    {/if}
    <section class="group" aria-labelledby="project-access">
      <h2 id="project-access">Models and routes</h2>
      <PublicationPanel projectId={selected.id} />
      <ProjectRouteGroupsPanel projectId={selected.id} {editable} />
    </section>
    <section class="group" aria-labelledby="project-end-users">
      <h2 id="project-end-users">End users</h2>
      <ProjectEndUserPolicyPanel projectId={selected.id} {editable} />
      <ProjectAttributionPolicyPanel projectId={selected.id} {editable} />
    </section>
    <section class="group" aria-labelledby="project-limits">
      <h2 id="project-limits">Spending limits</h2>
      <AggregateBudgetPanel projectId={selected.id} {editable} />
      <ProjectLimitTemplatesPanel projectId={selected.id} {editable} />
      <ProjectAttributionBudgetsPanel projectId={selected.id} {editable} />
    </section>
  {/key}
{/if}

{#if renaming}
  <Dialog title="Rename project" onClose={() => (renaming = false)}>
    {#snippet children(close)}
      <form class="dialog-form" onsubmit={submitRename}>
        <div class="form-field">
          <label for="project-rename">Name</label>
          <input
            id="project-rename"
            data-autofocus
            bind:value={renameValue}
            maxlength="100"
            required
            disabled={renameBusy}
          />
        </div>
        {#if actionProblem}<p class="inline-problem" role="alert">
            {actionProblem}
          </p>{/if}
        <div class="page-actions">
          <button class="button button-secondary" type="button" onclick={close}
            >Cancel</button
          >
          <button
            class="button button-primary"
            type="submit"
            disabled={renameBusy || !renameValue.trim()}
            >{renameBusy ? 'Saving…' : 'Save'}</button
          >
        </div>
      </form>
    {/snippet}
  </Dialog>
{/if}

<style>
  .group {
    display: grid;
    gap: 1rem;
    margin-top: 2.5rem;
  }
  .dialog-form {
    display: grid;
    gap: 1rem;
  }
  .dialog-form .page-actions {
    justify-content: flex-end;
  }
  .group > h2 {
    font-size: var(--text-heading);
  }
</style>
