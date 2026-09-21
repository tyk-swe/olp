<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { listProjectMemberships } from '$lib/features/access/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
  import { useRole } from '$lib/features/access/session/useRole.svelte';

  let {
    value = $bindable(''),
    id = 'project-scope',
    disabled = false
  }: {
    value: string;
    id?: string;
    disabled?: boolean;
  } = $props();

  const access = useRole();
  const globalScope = $derived(access.user?.access_scope !== 'assigned');
  const memberships = createQuery(() => ({
    queryKey: projectKeys.memberships,
    queryFn: ({ signal }) => listProjectMemberships(signal)
  }));
  const writable = $derived(
    (memberships.data ?? []).filter(
      (membership) => globalScope || membership.role === 'manager'
    )
  );

  $effect(() => {
    if (
      !globalScope &&
      writable.length &&
      !writable.some((membership) => membership.id === value)
    ) {
      value = writable[0].id;
    }
  });
</script>

<div class="form-field project-scope">
  <label for={id}>Project</label>
  {#if memberships.isPending}<span role="status">Loading projects…</span
    >{:else if memberships.isError}<span class="field-error" role="alert"
      >Projects are unavailable.
      <button
        class="text-button"
        type="button"
        onclick={() => memberships.refetch()}>Retry</button
      ></span
    >{:else}<select
      {id}
      bind:value
      {disabled}
      required={!globalScope}
      aria-label="Project"
      >{#if globalScope}<option value="">Installation-wide</option
        >{/if}{#each writable as membership (membership.id)}<option
          value={membership.id}>{membership.name}</option
        >{/each}</select
    >{#if !globalScope && !writable.length}<small
        >Your project memberships are read-only. A project manager membership is
        required to create resources.</small
      >{/if}{/if}
</div>
