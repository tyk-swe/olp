<script lang="ts">
  import { untrack } from 'svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { resolve } from '$app/paths';
  import { listProviders } from '$lib/features/providers/api/providers';
  import { listProviderCredentials } from '$lib/features/providers/api/credentials';
  import { providerKeys } from '$lib/features/providers/providerKeys';
  import { listApiKeys } from '$lib/features/access/api-keys/api';
  import { apiKeyKeys } from '$lib/features/access/api-keys/apiKeyKeys';
  import { listAllProjectMembers } from '$lib/features/access/projects/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import {
    saveCodeAccount,
    saveCodePool,
    saveCodeRoute,
    saveCodeBudget,
    type CodeAccount,
    type CodePool,
    type CodeRoute
  } from '$lib/api/code-mode';
  import {
    nativeModels,
    tokenLimit,
    mutationError,
    type CodeEditing
  } from './presentation';
  import AccountEnrollment from './AccountEnrollment.svelte';

  let {
    editing,
    projectId,
    accounts,
    pools,
    routes,
    allowed,
    onSaved,
    onCancel
  }: {
    editing: CodeEditing;
    projectId: string;
    accounts: CodeAccount[];
    pools: CodePool[];
    routes: CodeRoute[];
    allowed: boolean;
    onSaved: () => Promise<void>;
    onCancel: () => void;
  } = $props();
  const access = useRole();
  const initial = untrack(() => editing);
  let name = $state(
    initial.kind === 'accounts' || initial.kind === 'pools'
      ? (initial.current?.name ?? '')
      : ''
  );
  let providerId = $state(
    initial.kind === 'accounts' ? (initial.current?.provider_id ?? '') : ''
  );
  let credentialId = $state(
    initial.kind === 'accounts' ? (initial.current?.credential_id ?? '') : ''
  );
  let models = $state(
    initial.kind === 'accounts' || initial.kind === 'routes'
      ? (initial.current?.models.join('\n') ?? '')
      : ''
  );
  let enabled = $state(
    initial.kind !== 'pools' ? (initial.current?.enabled ?? true) : true
  );
  let kind = $state<'personal' | 'shared'>(
    initial.kind === 'pools'
      ? (initial.current?.kind ?? 'personal')
      : 'personal'
  );
  let owner = $state(
    initial.kind === 'pools'
      ? (initial.current?.owner_user_id ?? access.user?.id ?? '')
      : ''
  );
  let accountIds = $state(
    initial.kind === 'pools' ? [...(initial.current?.account_ids ?? [])] : []
  );
  let keyIds = $state(
    initial.kind === 'pools' ? [...(initial.current?.api_key_ids ?? [])] : []
  );
  let slug = $state(
    initial.kind === 'routes' ? (initial.current?.slug ?? '') : ''
  );
  let poolId = $state(
    initial.kind === 'routes' ? (initial.current?.pool_id ?? '') : ''
  );
  let maxBodyBytes = $state(
    initial.kind === 'routes'
      ? (initial.current?.max_body_bytes?.toString() ?? '')
      : ''
  );
  let routeId = $state(
    initial.kind === 'budgets' ? (initial.current?.route_id ?? '') : ''
  );
  let keyId = $state(
    initial.kind === 'budgets' ? (initial.current?.api_key_id ?? '') : ''
  );
  let daily = $state(
    initial.kind === 'budgets'
      ? (initial.current?.daily_tokens?.toString() ?? '')
      : ''
  );
  let monthly = $state(
    initial.kind === 'budgets'
      ? (initial.current?.monthly_tokens?.toString() ?? '')
      : ''
  );
  let busy = $state(false);
  let enrolling = $state(false);
  let error = $state('');

  const providers = createQuery(() => ({
    queryKey: providerKeys.all(),
    queryFn: ({ signal }) => listProviders(signal),
    enabled: editing.kind === 'accounts'
  }));
  const credentials = createQuery(() => ({
    queryKey: providerKeys.credentials(providerId),
    queryFn: ({ signal }) => listProviderCredentials(providerId, signal),
    enabled: editing.kind === 'accounts' && !!providerId
  }));
  const keys = createQuery(() => ({
    queryKey: apiKeyKeys.list(),
    queryFn: ({ signal }) => listApiKeys(signal),
    enabled:
      (editing.kind === 'pools' || editing.kind === 'budgets') &&
      access.can('api_keys.read')
  }));
  const members = createQuery(() => ({
    queryKey: projectKeys.allMembers(projectId),
    queryFn: ({ signal }) => listAllProjectMembers(projectId, signal),
    enabled: editing.kind === 'pools'
  }));
  const projectKeysList = $derived(
    (keys.data ?? []).filter((key) => key.project_id === projectId)
  );
  const eligibleKeys = $derived(
    projectKeysList.filter(
      (key) =>
        editing.kind !== 'pools' ||
        kind === 'shared' ||
        key.created_by === owner
    )
  );
  const selectedUnavailableKeys = $derived(
    keyIds.filter((id) => !eligibleKeys.some((key) => key.id === id))
  );
  const selectedUnavailableAccounts = $derived(
    accountIds.filter((id) => !accounts.some((account) => account.id === id))
  );
  const referenceError = $derived(
    editing.kind === 'accounts'
      ? providers.error || credentials.error
      : editing.kind === 'pools'
        ? keys.error || members.error
        : editing.kind === 'budgets'
          ? keys.error
          : null
  );

  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (busy || enrolling || !allowed) return;
    busy = true;
    error = '';
    try {
      switch (editing.kind) {
        case 'accounts':
          await saveCodeAccount(
            {
              project_id: projectId,
              name,
              provider_id: providerId,
              credential_id: credentialId,
              models: nativeModels(models),
              enabled
            },
            editing.current
          );
          break;
        case 'pools':
          if (accountIds.length > 100 || keyIds.length > 100)
            throw new Error(
              'A pool accepts at most 100 accounts and 100 explicit keys.'
            );
          await saveCodePool(
            {
              project_id: projectId,
              name,
              kind,
              owner_user_id: kind === 'personal' ? owner : null,
              account_ids: accountIds,
              api_key_ids: keyIds
            },
            editing.current
          );
          break;
        case 'routes':
          if (!/^[a-z0-9][a-z0-9._-]{0,99}$/.test(slug))
            throw new Error(
              'Use a route slug of 1–100 lowercase letters, numbers, dots, underscores or hyphens, starting with a letter or number.'
            );
          if (
            maxBodyBytes &&
            (!/^\d+$/.test(maxBodyBytes) ||
              Number(maxBodyBytes) < 1 ||
              Number(maxBodyBytes) > 1073741824)
          )
            throw new Error(
              'Use a body limit from 1 to 1073741824 bytes, or leave it empty.'
            );
          await saveCodeRoute(
            {
              project_id: projectId,
              slug,
              pool_id: poolId,
              max_body_bytes: maxBodyBytes ? Number(maxBodyBytes) : null,
              models: nativeModels(models),
              enabled
            },
            editing.current
          );
          break;
        case 'budgets': {
          const dailyTokens = tokenLimit(daily),
            monthlyTokens = tokenLimit(monthly);
          if (dailyTokens === null && monthlyTokens === null)
            throw new Error('Set at least one daily or monthly token limit.');
          await saveCodeBudget(
            {
              project_id: projectId,
              route_id: routeId || null,
              api_key_id: keyId || null,
              daily_tokens: dailyTokens,
              monthly_tokens: monthlyTokens,
              enabled
            },
            editing.current
          );
          break;
        }
      }
      await onSaved();
    } catch (e) {
      error = mutationError(e);
    } finally {
      busy = false;
    }
  }
</script>

<section class="card editor" aria-label="Code-mode editor">
  <h2>
    {editing.current ? 'Edit' : 'Create'}
    {editing.kind === 'accounts'
      ? 'account'
      : editing.kind === 'pools'
        ? 'pool'
        : editing.kind === 'routes'
          ? 'route draft'
          : 'token budget'}
  </h2>
  {#if error}<p role="alert" class="field-error">{error}</p>{/if}
  {#if referenceError}<p role="alert" class="field-error">
      Some choices could not be loaded. Existing selections are retained. Cancel
      and reopen to retry.
    </p>{/if}
  <form class="form-field" onsubmit={save}>
    <fieldset disabled={busy || !allowed}>
      {#if editing.kind === 'accounts' || editing.kind === 'pools'}
        <label for="code-name">Name</label><input
          id="code-name"
          bind:value={name}
          required
          maxlength="100"
        />
      {/if}
      {#if editing.kind === 'accounts'}
        <label for="code-provider">Provider</label>
        <select
          id="code-provider"
          bind:value={providerId}
          disabled={!!editing.current || enrolling}
          required
          onchange={() => (credentialId = '')}
        >
          <option value="">Choose a provider</option>
          {#each (providers.data ?? []).filter((p) => p.project_id === projectId) as provider (provider.id)}<option
              value={provider.id}>{provider.name}</option
            >{/each}
          {#if providerId && !providers.data?.some((p) => p.id === providerId && p.project_id === projectId)}
            <option value={providerId}>{providerId} · current selection</option>
          {/if}
        </select>
        <p>
          <a href={resolve('/providers')}
            >Manage providers and grant authentication</a
          >. Code-mode does not require an inference probe or ordinary provider
          activation.
        </p>
        <label for="code-credential">Enrolled credential version</label>
        <select
          id="code-credential"
          bind:value={credentialId}
          required
          disabled={enrolling}
        >
          <option value="">Choose an enrolled grant</option>
          {#each (credentials.data ?? []).filter((c) => c.grant) as credential (credential.id)}
            <option value={credential.id}
              >Version {credential.version} · {credential.grant?.principal}
              {credential.revoked_at ? '· revoked' : ''}</option
            >
          {/each}
          {#if credentialId && !credentials.data?.some((c) => c.id === credentialId)}<option
              value={credentialId}>{credentialId} · current selection</option
            >{/if}
        </select>
        <p>
          Replacing a credential must preserve the observed principal. Existing
          trees never switch accounts.
        </p>
      {:else if editing.kind === 'pools'}
        <label for="code-kind">Pool kind</label>
        <select id="code-kind" bind:value={kind} disabled={!!editing.current}
          ><option value="personal">Personal</option><option value="shared"
            >Shared</option
          ></select
        >
        {#if kind === 'personal'}
          <label for="code-owner">Owner</label>
          <select
            id="code-owner"
            bind:value={owner}
            required
            disabled={!!editing.current}
          >
            <option value="">Choose a project member</option>
            {#each members.data ?? [] as member (member.user_id)}<option
                value={member.user_id}>{member.email}</option
              >{/each}
            {#if owner && !members.data?.some((m) => m.user_id === owner)}<option
                value={owner}>{owner}</option
              >{/if}
          </select>
          <p>
            Personal pools accept only keys issued by their owner. Pool kind and
            owner cannot change.
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
              />{account.name} · {account.eligible
                ? 'eligible'
                : 'ineligible'}</label
            >{/each}
          {#each selectedUnavailableAccounts as id (id)}<label class="choice"
              ><input type="checkbox" bind:group={accountIds} value={id} />{id} ·
              unavailable selection</label
            >{/each}
          {#if !accounts.length}<p>No accounts in this project yet.</p>{/if}
        </fieldset>
        <fieldset>
          <legend>Explicit API-key permissions</legend>
          {#each eligibleKeys as key (key.id)}<label class="choice"
              ><input
                type="checkbox"
                bind:group={keyIds}
                value={key.id}
              />{key.name} · {key.created_by_email}{key.revoked_at
                ? ' · revoked'
                : ''}</label
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
      {:else if editing.kind === 'routes'}
        <label for="code-slug">Route slug</label><input
          id="code-slug"
          bind:value={slug}
          required
          disabled={!!editing.current}
        />
        <label for="code-pool">Pool</label><select
          id="code-pool"
          bind:value={poolId}
          required
          ><option value="">Choose a pool</option
          >{#each pools as pool (pool.id)}<option value={pool.id}
              >{pool.name} · {pool.kind}</option
            >{/each}
          {#if poolId && !pools.some((p) => p.id === poolId)}
            <option value={poolId}>{poolId} · current selection</option>
          {/if}</select
        >
        <p>
          Saving changes only the draft. Publish to update the immutable route
          and its frozen provider connections. Republish after connection
          changes or adding an account from a new provider.
        </p>
      {:else}
        <label for="code-budget-route">Route scope</label><select
          id="code-budget-route"
          bind:value={routeId}
          disabled={!!editing.current}
          ><option value="">All project routes</option
          >{#each routes as route (route.id)}<option value={route.id}
              >{route.slug}</option
            >{/each}
          {#if routeId && !routes.some((r) => r.id === routeId)}
            <option value={routeId}>{routeId} · current selection</option>
          {/if}</select
        >
        <label for="code-budget-key">Key scope</label><select
          id="code-budget-key"
          bind:value={keyId}
          disabled={!!editing.current}
          ><option value="">All project keys</option
          >{#each projectKeysList as key (key.id)}<option value={key.id}
              >{key.name}</option
            >{/each}
          {#if keyId && !projectKeysList.some((key) => key.id === keyId)}
            <option value={keyId}>{keyId} · current selection</option>
          {/if}</select
        >
        <label for="code-daily">Daily hard token limit (UTC)</label><input
          id="code-daily"
          inputmode="numeric"
          bind:value={daily}
          placeholder="No daily cap"
        />
        <label for="code-monthly">Monthly hard token limit (UTC)</label><input
          id="code-monthly"
          inputmode="numeric"
          bind:value={monthly}
          placeholder="No monthly cap"
        />
        <p>
          Hard budgets are optional. Every matching budget reserves a proven
          safe bound before each generation. Operations without a proven bound
          are refused only when a matching hard budget is enabled. No output
          limit is injected.
        </p>
        <p>
          Earlier usage and unknown consumption count. Disabling a budget does
          not erase reservations. Scope is immutable; all matching route, key
          and project budgets apply together.
        </p>
      {/if}
      {#if editing.kind === 'accounts' || editing.kind === 'routes'}
        {#if editing.kind === 'routes'}
          <div class="form-field">
            <label for="code-max-body">Maximum request body (bytes)</label>
            <input
              id="code-max-body"
              inputmode="numeric"
              bind:value={maxBodyBytes}
              placeholder="Installation limit"
            />
            <small
              >Limits encoded and decoded requests and client WebSocket
              messages; cannot raise installation limits.</small
            >
          </div>
        {/if}
        <label for="code-models">Native models</label><textarea
          id="code-models"
          bind:value={models}
          required
          rows="3"
          placeholder="One native model identifier per line"></textarea>
        <p>
          Model names pass through unchanged. An allowed model is not proof of
          client or inference qualification.
        </p>
      {/if}
      {#if editing.kind !== 'pools'}<label class="choice"
          ><input
            type="checkbox"
            bind:checked={enabled}
          />Enabled{editing.kind === 'routes'
            ? ' in next publication'
            : ''}</label
        >{/if}
    </fieldset>
    <div class="actions">
      <button
        class="button button-primary"
        type="submit"
        disabled={busy || enrolling || !allowed}
        >{busy ? 'Saving…' : 'Save'}</button
      ><button
        class="button button-secondary"
        type="button"
        disabled={busy || enrolling}
        onclick={onCancel}>Cancel</button
      >
    </div>
  </form>
  {#if editing.kind === 'accounts' && providerId}
    {#key providerId}<AccountEnrollment
        {providerId}
        allowed={allowed && !busy}
        onComplete={(id) => (credentialId = id)}
        onBusyChange={(value) => (enrolling = value)}
      />{/key}
  {/if}
</section>

<style>
  .editor,
  form,
  fieldset {
    display: grid;
    gap: 0.75rem;
  }
  .editor {
    padding: 1.25rem;
  }
  fieldset {
    min-width: 0;
    padding: 0;
    margin: 0;
  }
  h2 {
    font-size: 1.25rem;
    font-weight: 600;
  }
  label,
  legend {
    font-size: 0.875rem;
    font-weight: 600;
  }
  p {
    color: var(--foreground-subtle);
    font-size: 0.875rem;
    line-height: 1.6;
  }
  .choice,
  .actions {
    display: flex;
    align-items: center;
    gap: 0.65rem;
  }
  .choice input {
    width: auto;
  }
  .field-error {
    color: var(--danger);
  }
</style>
