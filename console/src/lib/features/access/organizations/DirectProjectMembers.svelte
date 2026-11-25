<script lang="ts">
  import EntityPicker from '#lib/components/EntityPicker.svelte';
  import { personOptions } from '#lib/features/identity/index.js';
  import { errorMessage } from '#lib/management/http.js';
  import type { projectMembers } from './api';

  type ProjectMember = Awaited<ReturnType<typeof projectMembers>>[number];

  let {
    members,
    editable,
    busy,
    onRemove,
    onAdd
  }: {
    members: { isError: boolean; error: unknown; data?: ProjectMember[] };
    editable: boolean;
    busy: boolean;
    onRemove: (member: ProjectMember) => void;
    onAdd: (userId: string, role: 'manager' | 'viewer') => Promise<boolean>;
  } = $props();

  let projectUser = $state('');
  let projectRole = $state<'manager' | 'viewer'>('viewer');

  async function add(event: SubmitEvent) {
    event.preventDefault();
    if (await onAdd(projectUser.trim(), projectRole)) projectUser = '';
  }
</script>

<h3>Direct project members</h3>
<p>Organization memberships above apply in addition to this list.</p>
{#if members.isError}<p class="inline-problem" role="alert">
    {errorMessage(members.error)}
  </p>{/if}
{#each members.data ?? [] as m (m.user_id)}<p>
    {m.email} · {m.project_role}{#if editable}<button
        class="button button-secondary"
        disabled={busy}
        onclick={() => onRemove(m)}
      >
        Remove project member {m.email}
      </button>{/if}
  </p>{/each}
{#if editable}<form onsubmit={add}>
    <label class="form-field">
      Person<EntityPicker
        id="organization-project-member"
        source={personOptions}
        required
        bind:value={projectUser}
        emptyLabel="Choose someone"
        disabled={busy}
      />
    </label><label class="form-field">
      Role<select bind:value={projectRole} disabled={busy}>
        <option value="viewer">Viewer</option><option value="manager">
          Manager
        </option>
      </select>
    </label><button class="button button-primary" disabled={busy}>
      Add to project
    </button>
  </form>{/if}
