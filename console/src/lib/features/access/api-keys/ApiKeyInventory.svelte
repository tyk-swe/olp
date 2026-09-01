<script lang="ts">
  import { resolve } from '$app/paths';
  import { queryKeys } from '$lib/api/queryKeys';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { cursorPaginationProps } from '$lib/api/pagination';
  import {
    listApiKeyPage,
    revokeApiKey,
    rotateApiKey,
    type ApiKey,
    type ApiKeySecret
  } from '$lib/api/management/api-keys';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import { formatBudget, formatDate } from '$lib/format';
  import type { ApiKeyListState } from './apiKeyListState';

  let {
    listState,
    notice,
    submitError,
    canManage,
    onEdit,
    onSecret
  }: {
    listState: ApiKeyListState;
    notice: string;
    submitError: string;
    canManage: boolean;
    onEdit: (key: ApiKey) => void;
    onSecret: (secret: ApiKeySecret, preferredRoute?: string) => void;
  } = $props();

  const queryClient = useQueryClient();
  let busy = $state('');
  let mutationError = $state('');
  const keys = createQuery(() => ({
    queryKey: queryKeys.apiKeys.page(listState.cursor),
    queryFn: () => listApiKeyPage(listState.cursor)
  }));

  /** The overview setup checklist owns ['api-keys'] and goes stale otherwise. */
  async function refreshKeyConsumers() {
    await Promise.all([
      keys.refetch(),
      queryClient.invalidateQueries({ queryKey: queryKeys.apiKeys.all() })
    ]);
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
      await refreshKeyConsumers();
    } catch (error) {
      mutationError = errorMessage(error);
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
      await refreshKeyConsumers();
    } catch (error) {
      mutationError = errorMessage(error);
    } finally {
      busy = '';
    }
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
  {#if canManage}<a
      class="button button-primary"
      href={resolve('/api-keys/new')}>Create key <NavIcon name="arrow" /></a
    >{/if}
</div>
{#if !canManage}
  <ReadOnlyNote>
    Your role can view API keys but not create, edit, rotate, or revoke them.
  </ReadOnlyNote>
{/if}
{#if submitError || mutationError}<div class="inline-problem" role="alert">
    {submitError || mutationError}
  </div>{/if}
{#if notice}<div class="success-message" role="status">{notice}</div>{/if}

{#if keys.isPending}
  <div class="loading-state" role="status">Loading API keys…</div>
{:else if keys.isError}
  <div class="inline-problem" role="alert">
    {errorMessage(keys.error)}
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
      {#if canManage}<a
          class="button button-primary"
          href={resolve('/api-keys/new')}>Create first key</a
        >{/if}
    </div>
  </section>
{:else}
  <div class="table-shell key-table">
    <table class="data-table">
      <thead
        ><tr
          ><th>Name / lookup ID</th><th>Status</th><th>Scope</th><th>Limits</th
          ><th>Budget</th><th>Creator / created</th><th
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
              ><br /><small
                >{key.expires_at
                  ? `Expires ${formatDate(key.expires_at)}`
                  : 'No expiry'}</small
              ><br /><small
                >{key.rotated_at
                  ? `Rotated ${formatDate(key.rotated_at)}`
                  : 'Never rotated'}</small
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
            <td>
              {#if key.budget.daily.limit !== null || key.budget.monthly.limit !== null}
                <small>
                  {#if key.budget.daily.limit !== null}Daily {formatBudget(
                      key.budget.daily.accrued
                    )} / {formatBudget(key.budget.daily.limit)}{/if}
                  {#if key.budget.daily.limit !== null && key.budget.monthly.limit !== null}<br
                    />{/if}
                  {#if key.budget.monthly.limit !== null}Monthly {formatBudget(
                      key.budget.monthly.accrued
                    )} / {formatBudget(key.budget.monthly.limit)}{/if}
                </small>
              {:else}
                <small>No cost budget</small>
              {/if}
            </td>
            <td
              ><strong>{key.created_by_email}</strong><br /><small
                >{formatDate(key.created_at)}</small
              ></td
            >
            <td
              ><div class="row-actions">
                <button
                  class="button button-secondary"
                  type="button"
                  onclick={() => onEdit(key)}
                  disabled={Boolean(busy)}
                  >{canManage &&
                  !key.revoked_at &&
                  (!key.expires_at || new Date(key.expires_at) >= new Date())
                    ? 'Edit'
                    : 'View'}</button
                >{#if canManage && !key.revoked_at}{#if !key.expires_at || new Date(key.expires_at) >= new Date()}<button
                      class="button button-secondary"
                      type="button"
                      onclick={() => rotate(key)}
                      disabled={Boolean(busy)}
                      >{busy === `rotate-${key.id}`
                        ? 'Rotating…'
                        : 'Rotate'}</button
                    >{/if}<button
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
    {...cursorPaginationProps(listState, keys.data?.nextCursor)}
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
