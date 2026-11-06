<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { holds } from '$lib/features/access/session/authorization';
  import { listProjectMemberships } from '$lib/features/access/projects/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
  import ExportSinksPanel from './ExportSinksPanel.svelte';
  import CapturePoliciesPanel from './CapturePoliciesPanel.svelte';
  import './observability.css';
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

<section
  class="card observability-panel"
  aria-labelledby="observability-heading"
>
  <p class="eyebrow">Observability</p>
  <h2 id="observability-heading">Export sinks and payload capture</h2>
  <p class="section-help">
    Durable metadata and usage export sinks deliver to HTTPS, OTLP logs or
    object storage. Bounded payload capture is opt-in, with optional pinned
    local redactors.
  </p>

  <ExportSinksPanel
    {canManageInstallation}
    {canManageProjects}
    {canMutate}
    memberships={memberships.data ?? []}
  />
  <CapturePoliciesPanel
    {canManageInstallation}
    {canManageProjects}
    {canMutate}
    {ownerOnly}
  />
</section>
