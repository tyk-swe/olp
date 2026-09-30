<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { listProjectMemberships } from '$lib/features/access/projects/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
  import { useRole } from '$lib/features/access/session/useRole.svelte';

  let {
    value = $bindable(''),
    id = 'project-scope',
    disabled = false,
    unassigned
  }: {
    value: string;
    id?: string;
    disabled?: boolean;
    /**
     * Whether the installation-wide (unassigned) boundary is offered. It
     * defaults to the member's installation reach; a resource whose
     * installation-wide form needs more narrows it.
     */
    unassigned?: boolean;
  } = $props();

  const access = useRole();
  const globalScope = $derived(access.user?.access_scope !== 'assigned');
  const offerUnassigned = $derived(unassigned ?? globalScope);
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
      !offerUnassigned &&
      writable.length &&
      !writable.some((membership) => membership.id === value)
    ) {
      value = writable[0].id;
    }
  });
</script>

<div class="form-field project-scope">
  <label for={id}>Project</label>
  {#if memberships.isPending}<span class="inline-status" role="status"
      >Loading projects…</span
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
      required={!offerUnassigned}
      aria-label="Project"
      >{#if offerUnassigned}<option value="">Installation-wide</option
        >{/if}{#each writable as membership (membership.id)}<option
          value={membership.id}>{membership.name}</option
        >{/each}</select
    >{#if !offerUnassigned && !writable.length}<small
        >{#if globalScope}No project exists yet, and installation-wide resources
          of this kind need an operator or owner.{:else}Your project memberships
          are read-only. A project manager membership is required to create
          resources.{/if}</small
      >{/if}{/if}
</div>
