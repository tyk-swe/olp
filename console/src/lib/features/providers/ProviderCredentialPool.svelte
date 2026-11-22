<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import { useAction, useMutation } from '#lib/editing/action.svelte.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { captureResult } from '#lib/editing/result.js';

  import { useRole } from '#lib/session/useRole.svelte.js';
  import CredentialSlotForm from './CredentialSlotForm.svelte';
  import { parseExternalReference } from './externalReference';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import type { Provider } from './providers';
  import GrantEnrollmentPanel from './GrantEnrollmentPanel.svelte';
  import {
    deleteCredentialSlot,
    listCredentialSlots,
    putCredentialSlot,
    validateCredentialSlot,
    type CredentialSlot as Slot
  } from './credentials';
  import { GrantEnrollmentState } from './grantEnrollment.svelte';
  import { parseManualModelNames } from './providerEditor';
  import { providerKeys } from './providerKeys';

  const saveMutation = useMutation();
  const validateAction = useAction();
  let {
    provider,
    canManage,
    grant = false,
    onChanged
  }: {
    provider: Provider;
    canManage: boolean;
    /** Set when a grant authenticates the provider: its slots' credential
     * versions come from grant enrollment, never a pasted credential. */
    grant?: boolean;
    onChanged: () => void | Promise<void>;
  } = $props();
  const queryClient = useQueryClient();

  let error = $state('');
  let notice = $state('');
  /** The grant enrollment an operator is signing in through, and the slot
   * whose grant it re-enrolls. */
  const access = useRole();
  const grantPrincipal = access.user?.id;
  const grantState = new GrantEnrollmentState({
    allowed: () => canManage && access.user?.id === grantPrincipal,
    error: (message) => (error = message),
    completed: async (completion, { isCurrent }) => {
      notice = `${enrollmentSlot}: grant enrolled for ${completion.principal} as credential version ${completion.credential_version}, pending activation. Validate its access, then test and activate the provider.`;
      await queryClient.invalidateQueries(
        {
          queryKey: providerKeys.credentials(provider.id)
        },
        { throwOnError: true }
      );
      if (!isCurrent()) return;
      await onChanged();
      if (isCurrent()) await pool.refetch();
    }
  });
  const enrollment = $derived(grantState.enrollment);
  const busy = $derived(
    saveMutation.busy
      ? 'save'
      : validateAction.busy
        ? validateAction.key
        : grantState.busy
  );
  let enrollmentSlot = $state('');
  let editing = $state<Slot | null>(null);
  let editingEtag = $state('');
  let secret = $state('');
  let useExternal = $state(false);
  let externalValue = $state('');
  let models = $state('');
  let routes = $state('');
  let keys = $state('');
  usePrincipalLifetime(() => {
    clearDraft();
    error = notice = '';
  });
  const pool = createQuery(() => ({
    queryKey: providerKeys.slots(provider.id, provider.etag),
    queryFn: ({ signal }) => listCredentialSlots(provider.id, signal)
  }));
  function clearDraft() {
    saveMutation.reset();
    secret = externalValue = editingEtag = '';
    models = routes = keys = '';
    useExternal = false;
    editing = null;
  }
  function cancelEdit() {
    if (!busy) clearDraft();
  }
  function edit(slot?: Slot) {
    if (!canManage || busy) return;
    clearDraft();
    editingEtag = pool.data?.etag ?? '';
    editing = slot
      ? structuredClone(slot)
      : {
          id: crypto.randomUUID(),
          name: '',
          enabled: true,
          priority: 0,
          weight: 1,
          credential_version_id: null,
          allowed_models: [],
          allowed_routes: [],
          allowed_api_keys: [],
          requests_per_minute: null,
          tokens_per_minute: null,
          input_tokens_per_minute: null,
          output_tokens_per_minute: null,
          max_concurrency: null
        };
    secret = '';
    useExternal = false;
    externalValue = '';
    models = slot?.allowed_models?.join(', ') ?? '';
    routes = slot?.allowed_routes?.join(', ') ?? '';
    keys = slot?.allowed_api_keys?.join(', ') ?? '';
  }
  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (!canManage || busy || !editing || !editingEtag) return;
    notice = '';
    const external = useExternal
      ? captureResult(() => parseExternalReference(externalValue))
      : null;
    if (external && !external.ok) {
      saveMutation.error = errorMessage(external.error);
      return;
    }
    const input = [
      provider.id,
      editingEtag,
      {
        slot: {
          ...editing,
          // A write that names no credential version keeps the slot's.
          // Naming it would bind it again, which OLP refuses once it is
          // revoked or its grant lapsed.
          credential_version_id: null,
          allowed_models: parseManualModelNames(models),
          allowed_routes: parseManualModelNames(routes),
          allowed_api_keys: parseManualModelNames(keys)
        },
        credential: useExternal ? null : secret || null,
        ...(external?.ok ? { credential_reference: external.value } : {})
      }
    ] as const;
    await saveMutation.submit(
      input,
      (payload, requestId) => putCredentialSlot(...payload, requestId),
      async (_value, current) => {
        clearDraft();
        notice =
          'Credential staged. Validate its model access, then test and activate the completed provider draft.';
        await onChanged();
        if (!current()) return;
        await pool.refetch({ throwOnError: true });
      },
      {
        key: 'save',
        failure: (e, stage) =>
          stage === 'accept'
            ? `Credential staged, but refreshing failed. ${errorMessage(e)}`
            : errorMessage(e)
      }
    );
  }
  async function enroll(slot: Slot) {
    if (!canManage || busy || slot.id == null) return;
    notice = '';
    enrollmentSlot = slot.name;
    clearDraft();
    await grantState.start(provider, slot.id);
  }
  async function validate(slot: Slot) {
    if (!canManage || busy || !pool.data || slot.id == null) return;
    const slotId = slot.id;
    const etag = pool.data.etag;
    notice = '';
    await validateAction.run(
      () => validateCredentialSlot(provider.id, slotId, etag),
      async () => {
        notice = `${slot.name}: model access validated.`;
        await pool.refetch({ throwOnError: true });
      },
      { key: slotId, failure: (e) => errorMessage(e) }
    );
  }
</script>

<section class="card pool" aria-labelledby="credential-pool-heading">
  <h2 id="credential-pool-heading">Credential pool</h2>
  {#if grant}<p>
      Every slot signs in to the same upstream account; pool other accounts
      through other providers on a route. Re-enrolling a slot's grant with that
      account rotates its credential, while another account changes the
      provider's serving identity. Lower priorities are tried first; weights
      distribute requests within a priority.
    </p>{:else}<p>
      Use multiple accounts or keys on this connection. Lower priorities are
      tried first; weights distribute requests within a priority.
    </p>{/if}
  {#if saveMutation.error || validateAction.error || error}<p
      class="inline-problem"
      role="alert"
    >
      {saveMutation.error || validateAction.error || error}
    </p>{/if}
  {#if notice}<Callout tone="success">{notice}</Callout>{/if}
  {#if pool.isError}<p class="inline-problem" role="alert">
      Credentials could not be loaded. <button
        type="button"
        onclick={() => pool.refetch()}>Retry</button
      >
    </p>{/if}
  {#if pool.isPending}<p class="inline-status" role="status">
      Loading credentials…
    </p>{/if}
  {#if pool.data?.connection_usage}<p>
      Connection usage this minute: {pool.data.connection_usage
        .requests_this_minute} requests · {pool.data.connection_usage
        .tokens_this_minute} tokens · {pool.data.connection_usage
        .concurrent_requests} concurrent requests.
    </p>{/if}
  {#each Object.entries(pool.data?.shared_group_usage ?? {}) as [group, usage] (group)}<p
    >
      Shared token quota {group} this minute: {usage.tokens_this_minute} / {usage.tokens_per_minute}
      tokens across every member connection.
    </p>{/each}
  <ul>
    {#each pool.data?.items ?? [] as slot (slot.id)}
      {@const lapsed = Boolean(pool.data?.health?.[slot.id]?.lapsed)}
      <li>
        <div>
          <strong>{slot.name}</strong><span
            >{slot.enabled ? 'Enabled' : 'Disabled'} · Priority {slot.priority} ·
            Weight {slot.weight}</span
          >{#if pool.data?.health?.[slot.id]}{@const health =
              pool.data.health[slot.id]}<small
              ><span class:revoked={health.revoked || lapsed}
                >{health.revoked
                  ? grant
                    ? 'Revoked — re-enroll its grant'
                    : 'Revoked — stage a replacement secret'
                  : lapsed
                    ? 'Grant lapsed — re-enroll it'
                    : health.active_credential_version_id ===
                          slot.credential_version_id &&
                        slot.credential_version_id
                      ? 'Active'
                      : health.active_credential_version_id
                        ? 'Change staged'
                        : 'Draft'}</span
              >
              · {health.cooling_down
                ? 'Cooling down'
                : health.cooling_down === false
                  ? 'Available'
                  : 'Live health unknown'} · {health.validated_at
                ? 'Access validated'
                : 'Access validation required'}</small
            >{#if health.usage}<small
                >{health.usage.requests_this_minute} / {slot.requests_per_minute ??
                  'unlimited'} requests this minute · {health.usage
                  .tokens_this_minute} / {slot.tokens_per_minute ?? 'unlimited'} tokens{#if slot.input_tokens_per_minute != null}
                  · {health.usage.input_tokens_this_minute} / {slot.input_tokens_per_minute}
                  input tokens{/if}{#if slot.output_tokens_per_minute != null}
                  · {health.usage.output_tokens_this_minute} / {slot.output_tokens_per_minute}
                  output tokens{/if}
                · {health.usage.concurrent_requests} / {slot.max_concurrency ??
                  'unlimited'} concurrent</small
              >{/if}{/if}<small
            >{slot.allowed_models?.length || 'All'} models · {slot
              .allowed_routes?.length || 'All'} routes</small
          >
        </div>
        {#if canManage}<button
            class="button button-secondary"
            type="button"
            disabled={Boolean(busy)}
            onclick={() => edit(slot)}
            >{grant ? 'Edit' : 'Edit / rotate'}</button
          >{#if grant}<button
              class="button"
              class:button-primary={lapsed}
              class:button-secondary={!lapsed}
              type="button"
              disabled={Boolean(busy) || Boolean(enrollment)}
              onclick={() => enroll(slot)}
              >{busy === `enroll-${slot.id}`
                ? 'Starting sign-in…'
                : slot.credential_version_id
                  ? 'Re-enroll grant'
                  : 'Enroll grant'}</button
            >{/if}<button
            class="button button-secondary"
            type="button"
            disabled={Boolean(busy) || !slot.credential_version_id}
            onclick={() => validate(slot)}
            >{busy === slot.id ? 'Validating…' : 'Validate access'}</button
          >{/if}
        {#if slot.id && pool.data && access.allows('DELETE /api/v1/providers/{provider_id}/credential-slots/{slot_id}')}<ConfirmAction
            danger
            label="Delete slot"
            description={`Remove ${slot.name} from the provider credential pool`}
            identity={`${slot.id}:${pool.data.etag}`}
            disabled={Boolean(busy)}
            run={(requestId) =>
              deleteCredentialSlot(
                provider.id,
                slot.id ?? '',
                pool.data?.etag ?? '',
                requestId
              )}
            done={async () => {
              await onChanged();
              await pool.refetch();
            }}
          />{/if}
      </li>
    {/each}
  </ul>
  {#if enrollment}<GrantEnrollmentPanel
      profileId={provider.configuration.profile_id}
      {enrollment}
      bind:input={grantState.input}
      {busy}
      onContinue={() => grantState.continue()}
      onPoll={() => grantState.poll()}
      onCancel={() => grantState.cancel()}
    />{/if}
  {#if canManage && !editing}<button
      class="button button-secondary"
      type="button"
      disabled={!pool.data || pool.isFetching || Boolean(busy)}
      onclick={() => edit()}>Add credential</button
    >{/if}
  {#if editing}
    <CredentialSlotForm
      {canManage}
      slot={editing}
      {grant}
      bind:secret
      bind:useExternal
      bind:externalValue
      bind:models
      bind:routes
      bind:keys
      busy={Boolean(busy)}
      onSubmit={save}
      onCancel={cancelEdit}
    />
  {/if}
</section>

<style>
  .pool {
    padding: 1.5rem;
    margin-top: 1.5rem;
  }
  .revoked {
    color: var(--warning);
  }
  p,
  span,
  small {
    color: var(--foreground-muted);
  }
  ul {
    padding: 0;
    list-style: none;
  }
  li {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.65rem;
    padding: 0.85rem 0;
    border-top: 1px solid var(--border-hairline);
  }
  li div {
    display: grid;
    gap: 0.2rem;
    flex: 1;
    min-width: 12rem;
  }
</style>
