<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import FormField from '#lib/components/FormField.svelte';
  import { focusFormError } from '#lib/editing/focusError.js';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import QueryView from '#lib/components/QueryView.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { resolve } from '$app/paths';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { projectKeys } from '#lib/domain/projects/projectKeys.js';
  import { projectQueries } from '#lib/domain/projects/queries.js';
  import { listGroups, saveMapping, type Group } from './api';
  import {
    validateMapping,
    type MappingDraft,
    type Membership
  } from './mappingDraft';
  import { scimKeys } from './scimKeys';
  const roleLabels: Record<Exclude<MappingDraft['role'], ''>, string> = {
    viewer: 'Viewer',
    developer: 'Developer',
    operator: 'Operator',
    owner: 'Owner'
  };
  const scopeLabels: Record<MappingDraft['scope'], string> = {
    assigned: 'Assigned projects',
    global: 'All projects'
  };
  const projectRoleLabels: Record<Membership['role'], string> = {
    viewer: 'Viewer',
    manager: 'Manager'
  };
  const access = useRole();
  const client = useQueryClient();
  const groups = createQuery(() => ({
    queryKey: scimKeys.groups(),
    queryFn: ({ signal }) => listGroups(signal)
  }));
  const projects = createQuery(projectQueries.all);
  const editable = $derived(
    access.allows('PUT /api/v1/scim/groups/{scim_id}/mapping')
  );
  let current = $state<Group | null>(null);
  let role = $state<MappingDraft['role']>('');
  let scope = $state<MappingDraft['scope']>('assigned');
  let memberships = $state<MappingDraft['memberships']>([]);
  const write = useAction<string>();
  function edit(group: Group) {
    current = group;
    role = group.mapping.role ?? '';
    scope = group.mapping.accessScope ?? 'assigned';
    memberships = (group.mapping.projects ?? []).map((p) => ({
      ...p,
      id: crypto.randomUUID()
    }));
    write.reset();
  }
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    const form = event.currentTarget as HTMLFormElement;
    const group = current;
    if (!group || !editable || write.busy) return;
    const checked = validateMapping({ role, scope, memberships });
    if (!write.check(checked)) return focusFormError(form);
    const saved = await write.run(
      () => saveMapping(group, checked.value),
      async (result) => {
        edit(result);
        notify('Group access saved.');
        await client.invalidateQueries({ queryKey: scimKeys.groups() });
        await client.invalidateQueries({ queryKey: projectKeys.root });
      },
      { fallback: 'Could not save the group mapping.' }
    );
    if (!saved) await focusFormError(form);
  }
</script>

<PageTitle title="SCIM provisioning" />
<section class="card setup" aria-labelledby="scim-setup">
  <h2 id="scim-setup">Connect a directory</h2>
  <p>
    Use this installation’s public origin with <code>/scim/v2</code> as the base
    URL. Create an installation-wide management token with the
    <code>access</code>
    scope in
    <a href={resolve('identity/tokens')}>Management tokens</a>, and supply it as
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
<QueryView
  query={groups}
  loading="Loading provisioned groups…"
  failure="Could not load SCIM groups."
  emptyWhen={!groups.data?.length}
>
  {#snippet empty()}<p>
      No SCIM groups have been provisioned yet. Synchronize a group from your
      directory to configure its access here.
    </p>{/snippet}
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
            disabled={write.busy}
            onclick={() => edit(group)}
          >
            {editable ? 'Edit' : 'View'}
            {group.display_name}
          </button>
        </li>{/each}
    </ul>
  </section>
</QueryView>
{#if current}<form onsubmit={submit} aria-label="SCIM group mapping">
    <h2>{current.display_name}</h2>
    <p>
      Edits revoke affected managed-user sessions. A directory update preserves
      an omitted OLP extension; explicitly supplied mappings replace these
      values.
    </p>
    <fieldset disabled={!editable || write.busy}>
      <div class="form-field">
        <label for="scim-role">Additional installation role</label><select
          id="scim-role"
          bind:value={role}
        >
          <option value="">No additional role</option>
          {#each Object.entries(roleLabels) as [value, label] (value)}
            <option {value}>{label}</option>
          {/each}
        </select>
      </div>
      <div class="form-field">
        <label for="scim-scope">Access scope</label><select
          id="scim-scope"
          bind:value={scope}
        >
          {#each Object.entries(scopeLabels) as [value, label] (value)}
            <option {value}>{label}</option>
          {/each}
        </select><small>
          Installation operations still require their existing role. Global
          scope broadens access to all projects.
        </small>
      </div>
      <h3>Inherited project memberships</h3>
      {#each memberships as member (member.id)}<div class="membership">
          <FormField label="Project" error={write.fieldErrors[member.id]}>
            {#snippet control(attributes)}
              <select {...attributes} bind:value={member.value}>
                <option value="">Choose a project</option>

                {#each projects.data ?? [] as project (project.id)}
                  <option value={project.id}>{project.name}</option>
                {/each}
              </select>
            {/snippet}
          </FormField>
          <div class="form-field">
            <label for={`scim-project-role-${member.id}`}>
              Project role
            </label><select
              id={`scim-project-role-${member.id}`}
              bind:value={member.role}
            >
              {#each Object.entries(projectRoleLabels) as [value, label] (value)}
                <option {value}>{label}</option>
              {/each}
            </select>
          </div>
          <button
            type="button"
            class="button button-secondary"
            onclick={() =>
              (memberships = memberships.filter((p) => p.id !== member.id))}
          >
            Remove membership
          </button>
        </div>
      {/each}
      <button
        type="button"
        class="button button-secondary"
        disabled={memberships.length >= 100}
        onclick={() =>
          (memberships = [
            ...memberships,
            { id: crypto.randomUUID(), value: '', role: 'viewer' }
          ])}
      >
        Add project membership
      </button>
    </fieldset>
    {#if write.error}<p
        class="inline-problem"
        role="alert"
        tabindex="-1"
        data-error-summary
      >
        {write.error}
      </p>{/if}
    <div class="actions">
      {#if editable}<button class="button button-primary" disabled={write.busy}>
          {write.busy ? 'Saving…' : 'Save group access'}
        </button>{/if}<button
        type="button"
        class="button button-secondary"
        disabled={write.busy}
        onclick={() => (current = null)}
      >
        Close
      </button>
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
    align-items: start;
    gap: 1rem;
    margin-bottom: 1rem;
  }
  .membership > :global(.form-field) {
    flex: 1;
    min-width: 12rem;
  }
  .membership > button {
    margin-top: calc(1lh + 0.4rem);
    line-height: inherit;
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
