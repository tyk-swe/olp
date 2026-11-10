<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import type {
    CodeClientConfiguration,
    CodeClientSelection,
    CodeRoute
  } from '#lib/features/code-mode/api.js';
  import { listProjectMemberships } from '#lib/features/access/projects/api.js';
  import { projectKeys } from '#lib/features/access/projects/projectKeys.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import { authLifecycle } from '#lib/features/access/session/lifecycle.js';
  import {
    confirmDiscardChanges,
    guardUnsavedChanges
  } from '#lib/editing/unsavedChanges.js';
  import Advanced from '#lib/components/Advanced.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import AccountsSection from './AccountsSection.svelte';
  import AttemptsDiagnostics from './AttemptsDiagnostics.svelte';
  import BindingsDiagnostics from './BindingsDiagnostics.svelte';
  import BudgetsSection from './BudgetsSection.svelte';
  import PoolsSection from './PoolsSection.svelte';
  import RefusalsDiagnostics from './RefusalsDiagnostics.svelte';
  import RoutesSection from './RoutesSection.svelte';
  import TokenWindowsDiagnostics from './TokenWindowsDiagnostics.svelte';
  import { getCodeClientConfiguration } from './api.js';
  import { codeSections, type CodeSection } from './sections';

  let {
    loadClientConfiguration = (route, selection, signal) =>
      getCodeClientConfiguration(route, gatewayURL, selection, signal)
  }: {
    loadClientConfiguration?: (
      route: CodeRoute,
      selection: CodeClientSelection,
      signal?: AbortSignal
    ) => Promise<CodeClientConfiguration>;
  } = $props();
  let gatewayURL = $state(globalThis.location?.origin ?? '');
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
  const onDraftChange = (dirty: boolean) => {
    draftDirty = dirty;
  };
</script>

<PageHeader
  title="Code mode"
  description="Point coding tools like subscription-based assistants at an OLP route. Requests keep their native models and conversations, and paid targets need pricing and a spend limit."
/>
<div class="code-mode">
  {#if !readable}
    <EmptyState title="Nothing here for your role"
      >Ask an operator for access to code mode.</EmptyState
    >
  {:else if projects.isPending}<p class="loading-state" role="status">
      Loading projects…
    </p>
  {:else if projects.isError}
    <Callout tone="danger" title="Projects could not be loaded">
      Check your connection and try again.
      {#snippet actions()}<button
          class="button button-secondary"
          type="button"
          onclick={() => projects.refetch()}>Try again</button
        >{/snippet}
    </Callout>
  {:else if !projects.data?.length}
    <EmptyState title="No projects yet"
      >Code mode accounts, pools and routes belong to a project. Create a
      project first.</EmptyState
    >
  {:else}
    <div class="setup">
      <div class="form-field">
        <label for="code-project">Project</label><select
          id="code-project"
          value={projectId}
          onchange={changeProject}
          >{#each projects.data as project (project.id)}<option
              value={project.id}>{project.name}</option
            >{/each}</select
        >
      </div>
      <Advanced label="Gateway address">
        <div class="form-field">
          <label for="code-gateway-url">Public gateway URL</label>
          <input id="code-gateway-url" type="url" bind:value={gatewayURL} />
          <p>
            Change this when the gateway is served from a different address than
            this console. Client setup snippets use it.
          </p>
        </div>
      </Advanced>
    </div>
    <nav class="page-tabs" aria-label="Code mode sections">
      {#each codeSections as item (item.id)}<button
          type="button"
          aria-current={tab === item.id ? 'page' : undefined}
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
        {#if tab === 'accounts'}
          <AccountsSection {projectId} {allowed} {onDraftChange} />
        {:else if tab === 'pools'}
          <PoolsSection {projectId} {allowed} {onDraftChange} />
        {:else if tab === 'routes'}
          <RoutesSection
            {projectId}
            {allowed}
            {gatewayURL}
            {loadClientConfiguration}
            {onDraftChange}
          />
        {:else if tab === 'budgets'}
          <BudgetsSection {projectId} {allowed} {onDraftChange} />
        {:else if tab === 'bindings'}
          <BindingsDiagnostics {projectId} {allowed} />
        {:else if tab === 'attempts'}
          <AttemptsDiagnostics {projectId} />
        {:else if tab === 'refusals'}
          <RefusalsDiagnostics {projectId} />
        {:else}
          <TokenWindowsDiagnostics {projectId} />
        {/if}
      {/key}
    {/if}
  {/if}
</div>

<style>
  .code-mode {
    display: grid;
    gap: 1.5rem;
  }
  .setup {
    display: grid;
    gap: 1rem;
    max-width: 32rem;
  }
  .page-tabs {
    margin-top: 0;
  }
</style>
