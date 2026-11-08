<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import { holds } from '#lib/features/access/session/authorization.js';
  import { listProjectMemberships } from '#lib/features/access/projects/api.js';
  import { projectKeys } from '#lib/features/access/projects/projectKeys.js';
  import ExportSinksPanel from './ExportSinksPanel.svelte';
  import CapturePoliciesPanel from './CapturePoliciesPanel.svelte';
  import './observability.css';

  let { view }: { view: 'telemetry' | 'capture' } = $props();
  const access = useRole();
  const globalScope = $derived(access.user?.access_scope === 'global');
  const ownerOnly = $derived(access.user?.role === 'owner' && globalScope);
  const canManageInstallation = $derived(
    globalScope && holds(access.user, 'settings')
  );
  const canManageProjects = $derived(access.can('api_keys.manage'));
  const memberships = createQuery(() => ({
    queryKey: projectKeys.memberships,
    queryFn: ({ signal }) => listProjectMemberships(signal)
  }));
  const writableProjects = $derived(
    (memberships.data ?? [])
      .filter((m) => m.role === 'manager')
      .map((m) => m.id)
  );

  function canMutate(resourceProject: string | null | undefined) {
    if (resourceProject == null) return canManageInstallation;
    return canManageProjects && writableProjects.includes(resourceProject);
  }
</script>

<svelte:head
  ><title
    >{view === 'telemetry' ? 'Telemetry export' : 'Payload capture'} · OpenLLMProxy</title
  ></svelte:head
>

<div class="card observability-panel">
  {#if view === 'telemetry'}
    <ExportSinksPanel
      {canManageInstallation}
      {canManageProjects}
      {canMutate}
      memberships={memberships.data ?? []}
    />
  {:else}
    <CapturePoliciesPanel
      {canManageInstallation}
      {canManageProjects}
      {canMutate}
      {ownerOnly}
    />
  {/if}
</div>
