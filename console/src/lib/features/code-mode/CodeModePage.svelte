<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import type {
    CodeClientConfiguration,
    CodeClientSelection,
    CodeRoute
  } from '$lib/api/code-mode';
  import { listProjectMemberships } from '$lib/features/access/projects/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { authLifecycle } from '$lib/features/access/session/lifecycle';
  import {
    confirmDiscardChanges,
    guardUnsavedChanges
  } from '$lib/forms/unsavedChanges';
  import CodeManagement from './CodeManagement.svelte';
  import CodeDiagnostics from './CodeDiagnostics.svelte';
  import {
    codeSections,
    isManagementSection,
    type CodeSection
  } from './sections';

  let {
    gatewayURL,
    loadClientConfiguration
  }: {
    gatewayURL: string;
    loadClientConfiguration?: (
      route: CodeRoute,
      selection: CodeClientSelection,
      signal?: AbortSignal
    ) => Promise<CodeClientConfiguration>;
  } = $props();
  const access = useRole();
  const principal = $derived(
    access.user ? authLifecycle.principalPartition(access.user) : ''
  );
  const readable = $derived(access.allows('GET /api/v1/code/accounts'));
  const projects = createQuery(() => ({
    queryKey: projectKeys.memberships,
    queryFn: ({ signal }) => listProjectMemberships(signal),
    enabled: readable
  }));
  let projectId = $state('');
  let draftDirty = $state(false);
  guardUnsavedChanges(() => draftDirty);
  function changeProject(event: Event) {
    const select = event.currentTarget as HTMLSelectElement;
    if (select.value === projectId) return;
    if (confirmDiscardChanges(draftDirty)) {
      draftDirty = false;
      projectId = select.value;
    } else select.value = projectId;
  }
  let tab = $state<CodeSection>('accounts');
  $effect(() => {
    if (
      projects.data &&
      !projects.data.some((project) => project.id === projectId)
    )
      projectId = projects.data[0]?.id ?? '';
  });
  const allowed = $derived(
    access.allows('POST /api/v1/code/accounts') &&
      (access.globalScope ||
        projects.data?.some(
          (project) => project.id === projectId && project.role === 'manager'
        )) === true
  );
</script>

<svelte:head><title>Code mode · OpenLLMProxy</title></svelte:head>
<div class="code-mode">
  <header>
    <h1>Code mode</h1>
    <p>
      Use coding subscriptions or explicitly selected paid API targets through
      an OLP route and OLP API key. Requests retain native models and
      conversation ownership. Paid targets require pricing and spend
      authorization.
    </p>
  </header>
  {#if !readable}<p role="status">
      Your role cannot read code-mode configuration.
    </p>
  {:else if projects.isPending}<p role="status">Loading projects…</p>
  {:else if projects.isError}<p role="alert">
      Projects are unavailable. <button
        class="text-button"
        type="button"
        onclick={() => projects.refetch()}>Retry</button
      >
    </p>
  {:else if !projects.data?.length}<p>
      No project is available. Code-mode resources belong to a project.
    </p>
  {:else}
    <div class="project form-field">
      <label for="code-project">Project</label><select
        id="code-project"
        value={projectId}
        onchange={changeProject}
        >{#each projects.data as project (project.id)}<option value={project.id}
            >{project.name}</option
          >{/each}</select
      >
    </div>
    <nav aria-label="Code-mode sections">
      {#each codeSections as item (item.id)}<button
          type="button"
          class:active={tab === item.id}
          aria-pressed={tab === item.id}
          onclick={() => {
            if (tab !== item.id && confirmDiscardChanges(draftDirty)) {
              draftDirty = false;
              tab = item.id;
            }
          }}>{item.label}</button
        >{/each}
    </nav>
    {#if projectId}
      {#key `${principal}:${projectId}:${tab}`}
        {#if isManagementSection(tab)}
          <CodeManagement
            {projectId}
            kind={tab}
            {allowed}
            {gatewayURL}
            {loadClientConfiguration}
            onDraftChange={(dirty) => (draftDirty = dirty)}
          />
        {:else}<CodeDiagnostics {projectId} kind={tab} {allowed} />{/if}
      {/key}
    {/if}
  {/if}
</div>

<style>
  .code-mode {
    display: grid;
    gap: 1.5rem;
  }
  h1 {
    font-size: 2rem;
    font-weight: 650;
  }
  header p {
    max-width: 60rem;
    margin-top: 0.5rem;
    color: var(--foreground-subtle);
    line-height: 1.6;
  }
  .project {
    display: grid;
    gap: 0.5rem;
    max-width: 26rem;
  }
  label {
    font-size: 0.875rem;
    font-weight: 600;
  }
  nav {
    display: flex;
    gap: 0.5rem;
    flex-wrap: wrap;
    border-bottom: 1px solid var(--border);
    padding-bottom: 1rem;
  }
  nav button {
    padding: 0.6rem 0.8rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    color: var(--foreground-subtle);
  }
  nav button.active {
    color: var(--signal);
    border-color: var(--signal);
  }
</style>
