<script lang="ts">
  import { confirmAction } from '#lib/components/confirm.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { createQuery } from '@tanstack/svelte-query';
  import { providerKeys } from './providerKeys';
  import {
    networkCredentialPath,
    type ConfigurationDraft
  } from './configurationDraft.svelte';
  import type { ProviderEditor } from './providerEditor.svelte';
  import { listNetworkCredentials } from './providerProfiles';
  import { formatDate } from '#lib/format.js';

  let {
    editor,
    draft,
    idPrefix,
    disabled,
    onChange
  }: {
    editor: ProviderEditor;
    draft: ConfigurationDraft;
    idPrefix: string;
    disabled: boolean;
    onChange: () => void;
  } = $props();
  const editorView = $derived(editor.view);
  const provider = $derived(editor.snapshot.data?.provider);
  let secret = $state('');
  const editingDisabled = $derived(disabled || editorView.refreshPending);
  usePrincipalLifetime(() => {
    secret = '';
  });
  const credentials = createQuery(() => ({
    queryKey: providerKeys.networkCredentials(provider?.id ?? ''),
    queryFn: ({ signal }) => listNetworkCredentials(provider?.id ?? '', signal),
    enabled: Boolean(provider)
  }));
  const selected = $derived(draft.text(networkCredentialPath));
  const missing = $derived(
    selected &&
      !credentials.data?.some((credential) => credential.id === selected)
  );
  async function create() {
    if (editingDisabled) return;
    const submitted = secret;
    if (await editor.stageNetworkCredential(submitted)) {
      if (secret === submitted) secret = '';
    }
  }
  async function revoke(id: string) {
    if (editingDisabled) return;
    if (
      !(await confirmAction({
        title: 'Revoke this network credential?',
        body: 'Provider revisions that use it stop sending requests right away.',
        confirmLabel: 'Revoke',
        danger: true
      }))
    )
      return;
    await editor.revokeNetworkCredential(id);
  }
</script>

<div class="network-credentials">
  {#if editorView.refreshPending}<p class="inline-problem" role="status">
      The network credential change succeeded. Refresh the view before another
      change.
      <button
        type="button"
        class="button button-secondary"
        {disabled}
        onclick={editor.retryRefresh}>Retry refresh</button
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
          networkCredentialPath,
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
