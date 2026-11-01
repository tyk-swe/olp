<script lang="ts">
  import { untrack } from 'svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { resolve } from '$app/paths';
  import { listApiKeys } from '$lib/features/access/api-keys/api';
  import { apiKeyKeys } from '$lib/features/access/api-keys/apiKeyKeys';
  import { listAllProjectMembers } from '$lib/features/access/projects/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import {
    saveCodePool,
    type CodeAccount,
    type CodePool,
    type CodePoolWrite
  } from '$lib/api/code-mode';
  import { adapterLabel } from './presentation';

  import {
    useResourceSubmission,
    type ResourceEditorProps
  } from './resourceSubmission.svelte';
  import ResourceEditorForm from './ResourceEditorForm.svelte';

  let {
    current,
    projectId,
    accounts,
    allowed,
    onDraftChange,
    onSaved,
    onCancel
  }: ResourceEditorProps<CodePool> & { accounts: CodeAccount[] } = $props();
  const access = useRole();
  const initial = untrack(() => current);
  let name = $state(initial?.name ?? '');
  let kind = $state<'personal' | 'shared'>(initial?.kind ?? 'personal');
  let owner = $state(
    initial?.owner_user_id ?? untrack(() => access.user?.id) ?? ''
  );
  let accountIds = $state([...(initial?.account_ids ?? [])]);
  let accountWeights = $state<Record<string, number>>(
    initial?.account_weights ?? {}
  );
  let keyIds = $state([...(initial?.api_key_ids ?? [])]);

  const keys = createQuery(() => ({
    queryKey: apiKeyKeys.list(),
    queryFn: ({ signal }) => listApiKeys(signal),
    enabled: access.can('api_keys.read')
  }));
  const members = createQuery(() => ({
    queryKey: projectKeys.allMembers(projectId),
    queryFn: ({ signal }) => listAllProjectMembers(projectId, signal),
    enabled: access.allows('GET /api/v1/projects/{project_id}/members')
  }));
  const projectKeysList = $derived(
    (keys.data ?? []).filter((key) => key.project_id === projectId)
  );
  const eligibleKeys = $derived(
    projectKeysList.filter(
      (key) => kind === 'shared' || key.created_by === owner
    )
  );
  const selectedUnavailableKeys = $derived(
    keyIds.filter((id) => !eligibleKeys.some((key) => key.id === id))
  );
  const selectedUnavailableAccounts = $derived(
    accountIds.filter((id) => !accounts.some((account) => account.id === id))
  );

  const submission = useResourceSubmission({
    allowed: () => allowed,
    creating: () => !current,
    onDraftChange: (dirty) => onDraftChange(dirty),
    onSaved: () => onSaved(),
    onCancel: () => onCancel(),
    build: (): CodePoolWrite => {
      if (accountIds.length > 100 || keyIds.length > 100)
        throw new Error(
          'A pool accepts at most 100 accounts and 100 explicit keys.'
        );
      return {
        project_id: projectId,
        name,
        kind,
        owner_user_id: kind === 'personal' ? owner : null,
        account_ids: accountIds,
        account_weights: Object.fromEntries(
          accountIds.map((id) => [id, accountWeights[id] ?? 1])
        ),
        api_key_ids: keyIds
      };
    },
    persist: (input, requestId) => saveCodePool(input, current, requestId)
  });
</script>

<ResourceEditorForm
  title="pool"
  creating={!current}
  {allowed}
  {submission}
  referenceError={keys.error || members.error}
>
  <label for="code-name">Name</label><input
    id="code-name"
    bind:value={name}
    required
    maxlength="100"
  />
  <label for="code-kind">Pool kind</label>
  <select id="code-kind" bind:value={kind} disabled={!!current}
    ><option value="personal">Personal</option><option value="shared"
      >Shared</option
    ></select
  >
  {#if kind === 'personal'}
    <label for="code-owner">Owner</label>
    <select id="code-owner" bind:value={owner} required disabled={!!current}>
      <option value="">Choose a project member</option>
      {#each members.data ?? [] as member (member.user_id)}<option
          value={member.user_id}>{member.email}</option
        >{/each}
      {#if owner && !members.data?.some((m) => m.user_id === owner)}<option
          value={owner}>{owner}</option
        >{/if}
    </select>
    <p>
      Personal pools accept only keys issued by their owner. Pool kind and owner
      cannot change.
    </p>
  {:else}<p>
      Shared pools serve only the explicitly assigned project keys below.
    </p>{/if}
  <fieldset>
    <legend>Assigned accounts</legend>
    {#each accounts as account (account.id)}<label class="choice"
        ><input
          type="checkbox"
          bind:group={accountIds}
          value={account.id}
        />{account.name} · {adapterLabel(account.adapter)} · {account.eligible
          ? 'eligible'
          : 'ineligible'}</label
      >
      {#if accountIds.includes(account.id)}
        <label for={`code-weight-${account.id}`}
          >Weight for {account.name}</label
        >
        <input
          id={`code-weight-${account.id}`}
          type="number"
          min="1"
          max="2147483647"
          step="1"
          value={accountWeights[account.id] ?? 1}
          oninput={(event) => {
            accountWeights = {
              ...accountWeights,
              [account.id]: event.currentTarget.valueAsNumber
            };
          }}
          required
        />
      {/if}
    {/each}
    {#each selectedUnavailableAccounts as id (id)}<label class="choice"
        ><input type="checkbox" bind:group={accountIds} value={id} />{id} · unavailable
        selection</label
      >{/each}
    {#if !accounts.length}<p>No accounts in this project yet.</p>{/if}
  </fieldset>
  <fieldset>
    <legend>Explicit API-key permissions</legend>
    {#each eligibleKeys as key (key.id)}<label class="choice"
        ><input type="checkbox" bind:group={keyIds} value={key.id} />{key.name} ·
        {key.created_by_email}{key.revoked_at ? ' · revoked' : ''}</label
      >{/each}
    {#each selectedUnavailableKeys as id (id)}<label class="choice"
        ><input type="checkbox" bind:group={keyIds} value={id} />{id} · unavailable
        or not owned by this owner; remove before saving</label
      >{/each}
    {#if !eligibleKeys.length}<p>
        No matching keys available. <a href={resolve('/api-keys')}
          >Manage OLP keys</a
        >.
      </p>{/if}
  </fieldset>
</ResourceEditorForm>
