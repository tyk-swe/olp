<script lang="ts">
  import EntityPicker from '#lib/components/EntityPicker.svelte';
  import { personOptions } from '#lib/features/access/entityOptions.js';
  import { errorMessage } from '#lib/management/http.js';
  import type { organizationMembers } from './api';

  type OrganizationMember = Awaited<
    ReturnType<typeof organizationMembers>
  >[number];

  let {
    members,
    editable,
    busy,
    onRemove,
    onAdd
  }: {
    members: { isError: boolean; error: unknown; data?: OrganizationMember[] };
    editable: boolean;
    busy: boolean;
    onRemove: (member: OrganizationMember) => void;
    onAdd: (userId: string, role: 'manager' | 'viewer') => Promise<boolean>;
  } = $props();

  let userId = $state('');
  let role = $state<'manager' | 'viewer'>('viewer');

  async function add(event: SubmitEvent) {
    event.preventDefault();
    if (await onAdd(userId.trim(), role)) userId = '';
  }
</script>

<section class="section" aria-labelledby="organization-members">
  <h2 id="organization-members">Organization members</h2>
  <p class="section-help">
    Managers inherit management of every contained project. Direct project
    memberships remain independent. Add existing users by their user ID.
  </p>
  {#if members.isError}<p class="inline-problem" role="alert">
      {errorMessage(members.error)}
    </p>{/if}
  {#each members.data ?? [] as m (m.user_id)}<p>
      {m.email} · {m.role} · installation {m.installation_role}
      {#if editable}<button
          class="button button-secondary"
          disabled={busy}
          onclick={() => onRemove(m)}>Remove {m.email}</button
        >{/if}
    </p>{/each}
  {#if editable}<form onsubmit={add}>
      <label class="form-field"
        >Person<EntityPicker
          id="organization-member"
          source={personOptions}
          required
          bind:value={userId}
          emptyLabel="Choose someone"
          disabled={busy}
        /></label
      ><label class="form-field"
        >Role<select bind:value={role} disabled={busy}
          ><option value="viewer">Viewer</option><option value="manager"
            >Manager</option
          ></select
        ></label
      ><button class="button button-primary" disabled={busy}
        >Add to organization</button
      >
    </form>{/if}
</section>
