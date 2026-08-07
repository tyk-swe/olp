<script lang="ts">
  import { onDestroy } from 'svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import {
    getProvider,
    listProviderCredentials,
    revokeProviderCredential,
    rotateProviderCredential,
    type Provider,
    type ProviderCredential,
    type ProviderKindCapability
  } from '$lib/api/management/providers';
  import { requiresCredential } from './providerEditor';
  import { invalidateProviderSummaries } from '../providerCache';
  import {
    installProviderWithModels,
    type RunProviderAction
  } from './providerDetailCoordination';

  let {
    current,
    providerSpec,
    busy,
    run,
    onAcceptProvider,
    onResetModelPage,
    onNotice
  }: {
    current: Provider;
    providerSpec: ProviderKindCapability | undefined;
    busy: string;
    run: RunProviderAction;
    onAcceptProvider: (provider: Provider) => void;
    onResetModelPage: () => void;
    onNotice: (message: string) => void;
  } = $props();

  const queryClient = useQueryClient();
  const credentials = createQuery(() => ({
    queryKey: ['provider-credentials', current.id],
    queryFn: ({ signal }) => listProviderCredentials(current.id, signal)
  }));
  let credentialValue = $state('');

  onDestroy(() => {
    credentialValue = '';
  });

  async function rotate(event: SubmitEvent) {
    event.preventDefault();
    if (!credentialValue) return;
    await run('rotate-credential', async () => {
      await rotateProviderCredential(current, credentialValue);
      credentialValue = '';
      const [updated] = await Promise.all([
        getProvider(current.id),
        credentials.refetch()
      ]);
      await installProviderWithModels(
        queryClient,
        updated,
        undefined,
        onAcceptProvider,
        onResetModelPage
      );
      await invalidateProviderSummaries(queryClient);
      onNotice(
        'Credential version staged. Test and activate the provider to publish it; the current runtime credential remains live until then.'
      );
    });
  }

  async function revoke(credential: ProviderCredential) {
    if (!confirm(`Revoke credential version ${credential.version}?`)) return;
    await run(`revoke-${credential.id}`, async () => {
      await revokeProviderCredential(current, credential.id);
      const [updated] = await Promise.all([
        getProvider(current.id),
        credentials.refetch()
      ]);
      onAcceptProvider(updated);
      await invalidateProviderSummaries(queryClient);
      onNotice(`Credential version ${credential.version} revoked.`);
    });
  }
</script>

<section class="card editor" aria-labelledby="credential-heading">
  <p class="eyebrow">Secrets</p>
  <h2 id="credential-heading">Credential versions</h2>
  <p class="muted">
    The API never returns secret material. Rotation selects a draft version; the
    runtime credential remains live until activation.
  </p>
  {#if providerSpec && !requiresCredential(providerSpec, current.auth_mode)}<div
      class="identity-note"
    >
      <strong
        >{providerSpec.auth_modes.find(
          (auth) => auth.mode === current.auth_mode
        )?.label}</strong
      ><span
        >Identity is supplied by this deployment; there is no encrypted
        credential version.</span
      >
    </div>{:else}<form class="credential-form" onsubmit={rotate}>
      <label class="sr-only" for="rotation-secret">New credential</label><input
        id="rotation-secret"
        type="password"
        autocomplete="new-password"
        bind:value={credentialValue}
        placeholder="New credential"
      /><button
        class="button button-secondary"
        type="submit"
        disabled={!credentialValue || Boolean(busy) || !providerSpec}
        >{busy === 'rotate-credential' ? 'Staging…' : 'Stage rotation'}</button
      >
    </form>{/if}
  {#if credentials.isPending}<p role="status">Loading versions…</p>{:else}<ul
      class="credential-list"
    >
      {#each credentials.data ?? [] as credential (credential.id)}<li>
          <span
            ><strong>Version {credential.version}</strong><small
              >{new Date(credential.created_at).toLocaleString()}</small
            ></span
          ><span
            class:success={credential.active}
            class:warning={credential.draft_selected && !credential.active}
            class:danger={Boolean(credential.revoked_at)}
            class="badge"
            >{credential.revoked_at
              ? 'revoked'
              : credential.active && credential.draft_selected
                ? 'runtime active · draft selected'
                : credential.active
                  ? 'runtime active'
                  : credential.draft_selected
                    ? 'pending activation'
                    : 'retired'}</span
          >{#if !credential.active && !credential.draft_selected && !credential.revoked_at}<button
              class="button button-secondary"
              type="button"
              onclick={() => revoke(credential)}
              disabled={Boolean(busy)}>Revoke</button
            >{/if}
        </li>{/each}
    </ul>{/if}
</section>

<style>
  .editor {
    max-width: 66rem;
    margin-top: 1.25rem;
    padding: clamp(1.15rem, 3vw, 1.75rem);
  }
  h2 {
    margin: 0 0 0.85rem;
    font-size: 1.15rem;
    font-weight: 750;
    letter-spacing: -0.025em;
  }
  .muted,
  .credential-list small {
    color: var(--foreground-muted);
  }
  .credential-form {
    display: flex;
    align-items: end;
    gap: 0.6rem;
  }
  .credential-form input {
    min-width: 0;
    min-height: 2.5rem;
    flex: 1;
    padding: 0.5rem 0.7rem;
    border: 1px solid var(--border-strong);
    border-radius: 0.375rem;
    background: var(--surface);
    color: var(--foreground);
  }
  .credential-list {
    margin: 1rem 0 0;
    padding: 0;
    list-style: none;
  }
  .credential-list li {
    display: flex;
    min-height: 3.5rem;
    align-items: center;
    gap: 0.6rem;
    border-top: 1px solid var(--border);
  }
  .credential-list li > span:first-child {
    display: grid;
    margin-right: auto;
  }
  .identity-note {
    display: grid;
    gap: 0.15rem;
    padding: 0.8rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
    background: var(--surface-subtle);
    color: var(--foreground-muted);
    font-size: 0.78rem;
  }
  .identity-note strong {
    color: var(--foreground);
  }
</style>
