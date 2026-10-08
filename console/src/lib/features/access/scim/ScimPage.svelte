<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { resolve } from '$app/paths';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { listProjects } from '$lib/features/access/projects/api';
  import { listGroups, saveMapping, type Group, type Mapping } from './api';
  const access = useRole();
  const client = useQueryClient();
  const key = ['scim', 'groups'] as const;
  const groups = createQuery(() => ({
    queryKey: key,
    queryFn: ({ signal }) => listGroups(signal)
  }));
  const projects = createQuery(() => ({
    queryKey: ['projects', 'scim-options'],
    queryFn: ({ signal }) => listProjects(signal)
  }));
  const editable = $derived(
    access.allows('PUT /api/v1/scim/groups/{scim_id}/mapping')
  );
  let current = $state<Group | null>(null);
  let role = $state<NonNullable<Mapping['role']> | ''>('');
  let scope = $state<'assigned' | 'global'>('assigned');
  let memberships = $state<
    { id: string; value: string; role: 'manager' | 'viewer' }[]
  >([]);
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');
  function edit(group: Group) {
    current = group;
    role = group.mapping.role ?? '';
    scope = group.mapping.accessScope ?? 'assigned';
    memberships = (group.mapping.projects ?? []).map((p) => ({
      ...p,
      id: crypto.randomUUID()
    }));
    error = '';
    notice = '';
  }
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!current || !editable || busy) return;
    if (
      memberships.some((p) => !p.value) ||
      new Set(memberships.map((p) => p.value)).size !== memberships.length
    ) {
      error = 'Choose each project at most once.';
      return;
    }
    busy = true;
    error = '';
    try {
      const result = await saveMapping(current, {
        ...(role ? { role } : {}),
        accessScope: scope,
        projects: memberships.map(({ value, role }) => ({ value, role }))
      });
      edit(result);
      notice = 'Group access saved.';
      await client.invalidateQueries({ queryKey: key });
      await client.invalidateQueries({ queryKey: ['projects'] });
    } catch (e) {
      error =
        e instanceof Error ? e.message : 'Could not save the group mapping.';
    } finally {
      busy = false;
    }
  }
</script>

<svelte:head><title>SCIM provisioning · OpenLLMProxy</title></svelte:head>
<div class="page-header">
  <div>
    <p class="eyebrow">Identity</p>
    <h1 class="page-title">SCIM provisioning</h1>
    <p class="page-description">
      Synchronize users and groups from your identity directory while preserving
      direct project memberships.
    </p>
  </div>
</div>
<section class="card setup" aria-labelledby="scim-setup">
  <h2 id="scim-setup">Connect a directory</h2>
  <p>
    Use this installation’s public origin with <code>/scim/v2</code> as the base
    URL. Create an installation-wide management token with the
    <code>access</code>
    scope in <a href={resolve('/access')}>Access → Tokens</a>, and supply it as
    the Bearer credential in your directory.
  </p>
  <p>
    Discovery is available at <code>/ServiceProviderConfig</code>,
    <code>/ResourceTypes</code>, and <code>/Schemas</code>. Users start with the
    viewer role and assigned project access. Passwords are managed separately;
    SCIM does not create sign-in credentials.
  </p>
  <p>
    Group grants below combine with base user grants. Removing a group removes
    its inherited access; direct memberships remain. Disabling a user revokes
    their sessions. An owner’s local takeover prevents further SCIM edits to
    that identity.
  </p>
</section>
{#if groups.isPending}<p role="status">
    Loading provisioned groups…
  </p>{:else if groups.isError}<p role="alert">Could not load SCIM groups.</p>
  <button class="button button-secondary" onclick={() => groups.refetch()}
    >Retry</button
  >{:else if !groups.data?.length}<p>
    No SCIM groups have been provisioned yet. Synchronize a group from your
    directory to configure its access here.
  </p>{:else}
  <section aria-labelledby="scim-groups">
    <h2 id="scim-groups">Provisioned groups</h2>
    <ul class="groups">
      {#each groups.data as group (group.id)}<li>
          <div>
            <strong>{group.display_name}</strong>
            <p>
              {group.member_count} managed members · {group.mapping.role ??
                'No additional role'} · {group.mapping.projects?.length ?? 0}
              {(group.mapping.projects?.length ?? 0) === 1
                ? 'project'
                : 'projects'}
            </p>
          </div>
          <button
            class="button button-secondary"
            disabled={busy}
            onclick={() => edit(group)}
            >{editable ? 'Edit' : 'View'} {group.display_name}</button
          >
        </li>{/each}
    </ul>
  </section>
{/if}
{#if current}<form onsubmit={submit} aria-label="SCIM group mapping">
    <h2>{current.display_name}</h2>
    <p>
      Edits revoke affected managed-user sessions. A directory update preserves
      an omitted OLP extension; explicitly supplied mappings replace these
      values.
    </p>
    <fieldset disabled={!editable || busy}>
      <div class="form-field">
        <label for="scim-role">Additional installation role</label><select
          id="scim-role"
          bind:value={role}
          ><option value="">No additional role</option><option value="viewer"
            >Viewer</option
          ><option value="developer">Developer</option><option value="operator"
            >Operator</option
          ><option value="owner">Owner</option></select
        >
      </div>
      <div class="form-field">
        <label for="scim-scope">Access scope</label><select
          id="scim-scope"
          bind:value={scope}
          ><option value="assigned">Assigned projects</option><option
            value="global">All projects</option
          ></select
        ><small
          >Installation operations still require their existing role. Global
          scope broadens access to all projects.</small
        >
      </div>
      <h3>Inherited project memberships</h3>
      {#each memberships as member (member.id)}<div class="membership">
          <div class="form-field">
            <label for={`scim-project-${member.id}`}>Project</label><select
              id={`scim-project-${member.id}`}
              bind:value={member.value}
              ><option value="">Choose a project</option
              >{#each projects.data ?? [] as project (project.id)}<option
                  value={project.id}>{project.name}</option
                >{/each}</select
            >
          </div>
          <div class="form-field">
            <label for={`scim-project-role-${member.id}`}>Project role</label
            ><select
              id={`scim-project-role-${member.id}`}
              bind:value={member.role}
              ><option value="viewer">Viewer</option><option value="manager"
                >Manager</option
              ></select
            >
          </div>
          <button
            type="button"
            class="button button-secondary"
            onclick={() =>
              (memberships = memberships.filter((p) => p.id !== member.id))}
            >Remove membership</button
          >
        </div>{/each}<button
        type="button"
        class="button button-secondary"
        disabled={memberships.length >= 100}
        onclick={() =>
          (memberships = [
            ...memberships,
            { id: crypto.randomUUID(), value: '', role: 'viewer' }
          ])}>Add project membership</button
      >
    </fieldset>
    {#if error}<p class="error-message" role="alert">
        {error}
      </p>{/if}{#if notice}<p role="status">
        {notice}
      </p>{/if}
    <div class="actions">
      {#if editable}<button class="button button-primary" disabled={busy}
          >{busy ? 'Saving…' : 'Save group access'}</button
        >{/if}<button
        type="button"
        class="button button-secondary"
        disabled={busy}
        onclick={() => (current = null)}>Close</button
      >
    </div>
  </form>{/if}

<style>
  .setup {
    padding: 1.25rem;
    margin-bottom: 1.5rem;
  }
  .groups {
    list-style: none;
    padding: 0;
  }
  .groups li {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 1rem;
    padding: 1rem 0;
    border-bottom: 1px solid var(--border);
  }
  fieldset {
    border: 0;
    padding: 0;
    margin: 0 0 1rem;
  }
  .membership {
    display: flex;
    flex-wrap: wrap;
    align-items: end;
    gap: 1rem;
    margin-bottom: 1rem;
  }
  .membership .form-field {
    flex: 1;
    min-width: 12rem;
  }
  form {
    display: grid;
    gap: 1rem;
    padding-top: 1rem;
  }
  fieldset {
    display: grid;
    gap: 1rem;
  }
  .setup p + p {
    margin-top: 0.75rem;
  }
  .setup h2,
  form h2 {
    font-size: 1.125rem;
    font-weight: 600;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.75rem;
  }
</style>
