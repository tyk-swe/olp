<script lang="ts">
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '$lib/forms/mutationIntent.svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import type { Provider } from './api/providers';
  import { providerKeys } from './providerKeys';
  import type { ConfigurationDraft } from './configurationDraft.svelte';
  import type { RunProviderAction } from './providerEditor';
  import {
    createNetworkCredential,
    listNetworkCredentials,
    revokeNetworkCredential
  } from './api/profiles';
  import { formatDate } from '$lib/format';
  import { parseNativeJSON, nativeObject } from '$lib/json/nativeJson';

  const createNetworkCredentialIntent = useMutationIntent();
  const revokeNetworkCredentialIntent = useMutationIntent();
  let {
    provider,
    draft,
    idPrefix,
    disabled,
    run,
    onChange,
    onProviderChanged
  }: {
    provider: Provider;
    draft: ConfigurationDraft;
    idPrefix: string;
    disabled: boolean;
    run: RunProviderAction;
    onChange: () => void;
    onProviderChanged: (mutation?: {
      previousEtag: string;
      etag: string;
    }) => Promise<void>;
  } = $props();
  let secret = $state('');
  let pendingMutation = $state<{ previousEtag: string; etag: string } | null>(
    null
  );
  const editingDisabled = $derived(disabled || Boolean(pendingMutation));
  usePrincipalLifetime(() => {
    secret = '';
    pendingMutation = null;
  });
  const credentials = createQuery(() => ({
    queryKey: providerKeys.networkCredentials(provider.id),
    queryFn: ({ signal }) => listNetworkCredentials(provider.id, signal)
  }));
  const selected = $derived(
    draft.text(['options', 'network', 'credential_id'])
  );
  const missing = $derived(
    selected &&
      !credentials.data?.some((credential) => credential.id === selected)
  );
  async function create() {
    if (editingDisabled) return;
    await run('network-credential', async (isCurrent) => {
      if (!nativeObject(parseNativeJSON(secret)))
        throw new Error('Enter a network credential JSON object.');
      const submission = createNetworkCredentialIntent.prepare(
        { providerId: provider.id, secret },
        () => [provider.id, provider.etag, secret] as const
      );
      const created = await createNetworkCredential(
        ...submission.input,
        submission.requestId
      );
      if (!isCurrent()) return;
      createNetworkCredentialIntent.reset();
      secret = '';
      draft.set(['options', 'network', 'credential_id'], created.credential_id);
      onChange();
      pendingMutation = {
        previousEtag: submission.input[1],
        etag: created.etag
      };
      await refreshNetworkCredentials(isCurrent);
    });
  }
  async function revoke(id: string) {
    if (editingDisabled) return;
    if (
      !confirm(
        'Revoke this network credential immediately? Active revisions using it will stop dispatching.'
      )
    )
      return;
    await run('revoke-network-credential', async (isCurrent) => {
      const submission = revokeNetworkCredentialIntent.prepare(
        { providerId: provider.id, credentialId: id },
        () => [provider.id, provider.etag, id] as const
      );
      const revoked = await revokeNetworkCredential(
        ...submission.input,
        submission.requestId
      );
      if (!isCurrent()) return;
      revokeNetworkCredentialIntent.reset();
      pendingMutation = {
        previousEtag: submission.input[1],
        etag: revoked.etag
      };
      await refreshNetworkCredentials(isCurrent);
    });
  }

  async function refreshNetworkCredentials(isCurrent: () => boolean) {
    if (!isCurrent() || !pendingMutation) return;
    await onProviderChanged(pendingMutation);
    if (!isCurrent()) return;
    await credentials.refetch({ throwOnError: true });
    if (isCurrent()) pendingMutation = null;
  }

  async function retryRefresh() {
    await run('network-credential-refresh', refreshNetworkCredentials);
  }
</script>

<div class="network-credentials">
  {#if pendingMutation}<p class="inline-problem" role="status">
      The network credential change succeeded. Refresh the view before another
      change.
      <button
        type="button"
        class="button button-secondary"
        {disabled}
        onclick={retryRefresh}>Retry refresh</button
      >
    </p>{/if}
  <div class="form-field">
    <label for={`${idPrefix}-credential`}>Network credential</label>
    <select
      id={`${idPrefix}-credential`}
      value={selected}
      disabled={editingDisabled}
      onchange={(event) => {
        draft.set(
          ['options', 'network', 'credential_id'],
          event.currentTarget.value || undefined
        );
        onChange();
      }}
    >
      <option value="">No proxy or client-certificate credential</option>
      {#if missing}<option value={selected}>Saved reference unavailable</option
        >{/if}
      {#each credentials.data ?? [] as credential (credential.id)}<option
          value={credential.id}
          disabled={Boolean(credential.revoked_at)}
          >Version {credential.version}{credential.revoked_at
            ? ' · revoked'
            : ''}</option
        >{/each}
    </select>
    <small
      >Only unrevoked credentials owned by this connection can be selected.
      Values are encrypted and never returned.</small
    >
  </div>
  {#if credentials.isError}<p class="inline-problem" role="alert">
      Network credentials could not be loaded. <button
        class="button button-secondary"
        type="button"
        onclick={() => credentials.refetch()}>Retry</button
      >
    </p>{/if}
  <details>
    <summary>Add or revoke network credentials</summary>
    <p>
      Store proxy authentication or a matching client certificate and key as a
      new encrypted version. Public trust roots belong in the connection fields.
    </p>
    <div class="form-field">
      <label for={`${idPrefix}-secret`}
        >Network credential JSON (write only)</label
      ><input
        id={`${idPrefix}-secret`}
        type="password"
        bind:value={secret}
        autocomplete="new-password"
        spellcheck="false"
        maxlength="65536"
        disabled={editingDisabled}
      />
    </div>
    <button
      class="button button-secondary"
      type="button"
      onclick={create}
      disabled={editingDisabled || !secret}
      >Store and select network credential</button
    >
    {#each credentials.data ?? [] as credential (credential.id)}<div
        class="credential-row"
      >
        <span
          >Version {credential.version} · {formatDate(
            credential.created_at
          )}{credential.revoked_at ? ' · revoked' : ''}</span
        >{#if !credential.revoked_at}<button
            class="button button-secondary"
            type="button"
            disabled={editingDisabled}
            onclick={() => revoke(credential.id)}
            >Revoke version {credential.version}</button
          >{/if}
      </div>{/each}
  </details>
</div>

<style>
  .network-credentials {
    margin-top: 1rem;
  }
  details {
    margin-top: 1rem;
  }
  summary {
    cursor: pointer;
    font-weight: 500;
  }
  p {
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .credential-row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 1rem;
    justify-content: space-between;
    margin-top: 0.75rem;
  }
  .network-credentials > details > button {
    margin-top: 0.75rem;
  }
</style>
