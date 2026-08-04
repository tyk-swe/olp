<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { popCursor, pushCursor } from '$lib/api/pagination';
  import {
    listApiKeyPage,
    revokeApiKey,
    rotateApiKey,
    type ApiKey,
    type ApiKeySecret
  } from '$lib/api/management/api-keys';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import type { ApiKeyListState } from './apiKeyListState';

  let {
    listState,
    notice,
    errorMessage,
    onEdit,
    onSecret
  }: {
    listState: ApiKeyListState;
    notice: string;
    errorMessage: string;
    onEdit: (key: ApiKey) => void;
    onSecret: (secret: ApiKeySecret, preferredRoute?: string) => void;
  } = $props();

  let busy = $state('');
  let mutationError = $state('');
  const keys = createQuery(() => ({
    queryKey: ['api-key-page', listState.cursor ?? 'first'],
    queryFn: () => listApiKeyPage(listState.cursor)
  }));

  function message(error: unknown) {
    return error instanceof Error
      ? error.message
      : 'The control API could not complete the request.';
  }

  async function rotate(key: ApiKey) {
    if (
      !confirm(
        `Rotate “${key.name}”? Existing clients stop authenticating when revocation converges.`
      )
    )
      return;
    busy = `rotate-${key.id}`;
    mutationError = '';
    try {
      onSecret(await rotateApiKey(key), key.allowed_routes[0]);
      await keys.refetch();
    } catch (error) {
      mutationError = message(error);
    } finally {
      busy = '';
    }
  }

  async function revoke(key: ApiKey) {
    if (!confirm(`Revoke “${key.name}”? This cannot be undone.`)) return;
    busy = `revoke-${key.id}`;
    mutationError = '';
    try {
      await revokeApiKey(key);
      await keys.refetch();
    } catch (error) {
      mutationError = message(error);
    } finally {
      busy = '';
    }
  }

  function nextPage() {
    const next = keys.data?.nextCursor;
    if (next) pushCursor(listState, next);
  }
</script>

<div class="page-header">
  <div>
    <p class="eyebrow">Access</p>
    <h1 class="page-title">API Keys</h1>
    <p class="page-description">
      Issue independent 32-byte proxy keys with scopes, route allowlists, and
      distributed hard limits.
    </p>
  </div>
  <a class="button button-primary" href={resolve('/api-keys/new')}
    >Create key <NavIcon name="arrow" /></a
  >
</div>
{#if errorMessage || mutationError}<div class="inline-problem" role="alert">
    {errorMessage || mutationError}
  </div>{/if}
{#if notice}<div class="success-message" role="status">{notice}</div>{/if}

{#if keys.isPending}
  <div class="loading-state" role="status">Loading API keys…</div>
{:else if keys.isError}
  <div class="inline-problem" role="alert">
    {message(keys.error)}
    <button
      class="button button-secondary"
      type="button"
      onclick={() => keys.refetch()}>Retry</button
    >
  </div>
{:else if !keys.data?.items.length && listState.history.length === 0}
  <section class="card empty-state">
    <div>
      <h2>No API keys</h2>
      <p>Create a scoped key after activating your first route.</p>
      <a class="button button-primary" href={resolve('/api-keys/new')}
        >Create first key</a
      >
    </div>
  </section>
{:else}
  <div class="table-shell key-table">
    <table class="data-table">
      <thead
        ><tr
          ><th>Name / lookup ID</th><th>Status</th><th>Scope</th><th>Limits</th
          ><th>Creator / created</th><th
            ><span class="sr-only">Actions</span></th
          ></tr
        ></thead
      >
      <tbody>
        {#each keys.data?.items ?? [] as key (key.id)}
          <tr>
            <td
              ><strong>{key.name}</strong><br /><code>{key.lookup_id}</code></td
            >
            <td
              ><span
                class:danger={Boolean(key.revoked_at)}
                class:warning={Boolean(
                  key.expires_at && new Date(key.expires_at) < new Date()
                )}
                class:success={!key.revoked_at &&
                  (!key.expires_at || new Date(key.expires_at) >= new Date())}
                class="badge"
                >{key.revoked_at
                  ? 'revoked'
                  : key.expires_at && new Date(key.expires_at) < new Date()
                    ? 'expired'
                    : 'active'}</span
              ></td
            >
            <td
              >{key.scopes.join(', ') || 'none'}<br /><small
                >{key.allowed_routes.length
                  ? key.allowed_routes.join(', ')
                  : 'all routes'}</small
              ></td
            >
            <td
              ><small
                >{key.requests_per_minute
                  ? `${key.requests_per_minute} RPM`
                  : 'unlimited RPM'}<br />{key.tokens_per_minute
                  ? `${key.tokens_per_minute} TPM`
                  : 'unlimited TPM'} · {key.max_concurrency
                  ? `${key.max_concurrency} concurrent`
                  : 'unlimited concurrency'}</small
              ></td
            >
            <td
              ><strong>{key.created_by_email}</strong><br /><small
                >{new Date(key.created_at).toLocaleDateString()}</small
              ></td
            >
            <td
              ><div class="row-actions">
                {#if !key.revoked_at && (!key.expires_at || new Date(key.expires_at) >= new Date())}<button
                    class="button button-secondary"
                    type="button"
                    onclick={() => onEdit(key)}
                    disabled={Boolean(busy)}>Edit</button
                  ><button
                    class="button button-secondary"
                    type="button"
                    onclick={() => rotate(key)}
                    disabled={Boolean(busy)}
                    >{busy === `rotate-${key.id}`
                      ? 'Rotating…'
                      : 'Rotate'}</button
                  ><button
                    class="button button-secondary danger-button"
                    type="button"
                    onclick={() => revoke(key)}
                    disabled={Boolean(busy)}
                    >{busy === `revoke-${key.id}`
                      ? 'Revoking…'
                      : 'Revoke'}</button
                  >{/if}
              </div></td
            >
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
  <CursorPagination
    page={listState.history.length + 1}
    hasPrevious={listState.history.length > 0}
    hasNext={Boolean(keys.data?.nextCursor)}
    onPrevious={() => popCursor(listState)}
    onNext={nextPage}
    label="API key pages"
  />
{/if}

<style>
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1.15rem;
    letter-spacing: -0.025em;
  }
  .success-message {
    margin: 1rem 0;
    padding: 0.8rem 1rem;
    border-radius: 0.375rem;
    background: var(--success-soft);
    color: var(--success);
    font-weight: 700;
  }
  .key-table {
    margin-top: 1.5rem;
  }
  code {
    font:
      0.72rem 'JetBrains Mono Variable',
      monospace;
  }
  td small {
    color: var(--foreground-muted);
  }
  .row-actions {
    display: flex;
    gap: 0.4rem;
  }
  .danger-button {
    color: var(--danger);
  }
</style>
