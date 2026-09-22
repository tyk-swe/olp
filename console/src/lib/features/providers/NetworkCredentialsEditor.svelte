<script lang="ts">
  import { onDestroy } from 'svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import type { Provider } from './api';
  import type { ConfigurationDraft } from './configurationDraft.svelte';
  import type { RunProviderAction } from './providerEditor';
  import {
    createNetworkCredential,
    listNetworkCredentials,
    revokeNetworkCredential
  } from './profiles';
  import { formatDate } from '$lib/format';
  import { parseNativeJSON, nativeObject } from '$lib/json/nativeJson';
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
    onProviderChanged: () => Promise<void>;
  } = $props();
  let secret = $state('');
  const credentials = createQuery(() => ({
    queryKey: ['providers', 'network-credentials', provider.id],
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
    await run('network-credential', async () => {
      if (!nativeObject(parseNativeJSON(secret)))
        throw new Error('Enter a network credential JSON object.');
      const created = await createNetworkCredential(
        provider.id,
        provider.etag,
        secret
      );
      secret = '';
      draft.set(['options', 'network', 'credential_id'], created.credential_id);
      onChange();
      await onProviderChanged();
      await credentials.refetch();
    });
  }
  async function revoke(id: string) {
    if (
      !confirm(
        'Revoke this network credential immediately? Active revisions using it will stop dispatching.'
      )
    )
      return;
    await run('revoke-network-credential', async () => {
      await revokeNetworkCredential(provider.id, provider.etag, id);
      await onProviderChanged();
      await credentials.refetch();
    });
  }
  onDestroy(() => {
    secret = '';
  });
</script>

<div class="network-credentials">
  <div class="form-field">
    <label for={`${idPrefix}-credential`}>Network credential</label>
    <select
      id={`${idPrefix}-credential`}
      value={selected}
      {disabled}
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
        {disabled}
      />
    </div>
    <button
      class="button button-secondary"
      type="button"
      onclick={create}
      disabled={disabled || !secret}>Store and select network credential</button
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
            {disabled}
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
