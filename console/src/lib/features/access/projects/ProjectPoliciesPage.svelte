<script lang="ts">
  import AggregateBudgetPanel from '../budgets/AggregateBudgetPanel.svelte';
  import ProjectLimitTemplatesPanel from './ProjectLimitTemplatesPanel.svelte';
  import ProjectAttributionBudgetsPanel from './ProjectAttributionBudgetsPanel.svelte';
  import ProjectRouteGroupsPanel from './ProjectRouteGroupsPanel.svelte';
  import ProjectAttributionPolicyPanel from './ProjectAttributionPolicyPanel.svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useRole } from '../session/useRole.svelte';
  import { listProjectMemberships } from './api';
  import { projectKeys } from './projectKeys';
  import ProjectEndUserPolicyPanel from './ProjectEndUserPolicyPanel.svelte';

  const access = useRole();
  const projects = createQuery(() => ({
    queryKey: projectKeys.memberships,
    queryFn: ({ signal }) => listProjectMemberships(signal)
  }));
  let selectedId = $state('');
  const selected = $derived(
    projects.data?.find((project) => project.id === selectedId)
  );
  const editable = $derived(
    access.allows('PUT /api/v1/projects/{project_id}/end-user-policy') &&
      (access.globalScope || selected?.role === 'manager')
  );
  $effect(() => {
    if (!projects.data?.some((project) => project.id === selectedId)) {
      selectedId = projects.data?.[0]?.id ?? '';
    }
  });
</script>

<svelte:head><title>Project policies · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div>
    <p class="eyebrow">Access</p>
    <h1 class="page-title">Project policies</h1>
    <p class="page-description">
      Manage end-user limits, attribution and route access shared across a
      project's keys.
    </p>
  </div>
</div>

{#if projects.isPending}
  <p role="status">Loading projects…</p>
{:else if projects.isError}
  <p class="inline-problem" role="alert">{errorMessage(projects.error)}</p>
  <button
    class="button button-secondary"
    type="button"
    onclick={() => projects.refetch()}>Retry projects</button
  >
{:else if !projects.data?.length}
  <p>You do not have access to any projects.</p>
{:else}
  <div class="form-field">
    <label for="policy-project">Project</label>
    <select id="policy-project" bind:value={selectedId}>
      {#each projects.data as project (project.id)}
        <option value={project.id}>{project.name}</option>
      {/each}
    </select>
  </div>
  {#if selected}
    {#if !editable}<p class="muted">
        You can view this policy. Editing requires key management permission and
        project manager access.
      </p>{/if}
    {#key selected.id}
      <ProjectEndUserPolicyPanel projectId={selected.id} {editable} />
      <ProjectAttributionPolicyPanel projectId={selected.id} {editable} />
      <AggregateBudgetPanel projectId={selected.id} {editable} />
      <ProjectRouteGroupsPanel projectId={selected.id} {editable} />
      <ProjectLimitTemplatesPanel projectId={selected.id} {editable} />
      <ProjectAttributionBudgetsPanel projectId={selected.id} {editable} />
    {/key}
  {/if}
{/if}
