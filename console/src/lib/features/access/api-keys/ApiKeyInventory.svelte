<script lang="ts">
  import { apiKeyQueries } from '$lib/features/access/api-keys/apiKeyQueries';

  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { cursorPaginationProps, resetCursor } from '$lib/lists/pagination';
  import {
    listApiKeyPage,
    revokeApiKey,
    rotateApiKey,
    type ApiKey,
    type ApiKeySecret
  } from '$lib/features/access/api-keys/api';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import { formatBudget, formatDate } from '$lib/format';
  import type { ApiKeyListState } from '$lib/features/access/api-keys/apiKeyListState';

  let {
    listState = $bindable(),
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
  const createdBy = $derived(
    page.url.searchParams.get('created_by')?.trim().toLowerCase() || undefined
  );
  let issuer = $state('');
  const cursor = $derived(
    listState.createdBy === createdBy ? listState.cursor : undefined
  );
  $effect(() => {
    issuer = createdBy ?? '';
    if (listState.createdBy !== createdBy) {
      resetCursor(listState);
      listState.createdBy = createdBy;
    }
  });
  const keys = createQuery(() => ({
    queryKey: apiKeyQueries.page(cursor, createdBy),
    queryFn: ({ signal }) => listApiKeyPage(cursor, signal, createdBy)
  }));

  const issuers = $derived(
    new Map(
      (keys.data?.items ?? []).map((key) => [
        key.created_by,
        key.created_by_email
      ])
    )
  );

  async function filterIssuer(value: string) {
    const url = new URL(page.url);
    const id = value.trim().toLowerCase();
    if (id) url.searchParams.set('created_by', id);
    else url.searchParams.delete('created_by');
    const target = url.search
      ? resolve(`/api-keys?${url.searchParams}${url.hash}`)
      : url.hash
        ? resolve(`/api-keys#${url.hash.slice(1)}`)
        : resolve('/api-keys');
    await goto(target, {
      keepFocus: true,
      noScroll: true
    });
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
      await queryClient.invalidateQueries({ queryKey: apiKeyQueries.root });
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
      await queryClient.invalidateQueries({ queryKey: apiKeyQueries.root });
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
      Manage client access with scoped API keys, route permissions, and usage
      limits.
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

<form
  class="card issuer-filter"
  onsubmit={(event) => {
    event.preventDefault();
    void filterIssuer(issuer);
  }}
>
  <label class="field">
    <span>Issuer (user ID)</span>
    <input
      bind:value={issuer}
      list="key-issuers"
      placeholder="All issuers"
      pattern={'[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}'}
      aria-describedby="issuer-hint"
    />
  </label>
  <datalist id="key-issuers">
    {#each [...issuers] as [id, email] (id)}<option value={id}>{email}</option
      >{/each}
  </datalist>
  <p id="issuer-hint">
    Choose an issuer from this page or enter a user ID. Keys attributed to
    inactive members remain available for review.
  </p>
  <div class="page-actions">
    <button class="button button-secondary" type="submit">Apply issuer</button>
    <button
      class="button button-secondary"
      type="button"
      onclick={() => filterIssuer('')}>Clear issuer</button
    >
  </div>
</form>

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
      <h2>{createdBy ? 'No API keys for this issuer' : 'No API keys'}</h2>
      <p>
        {createdBy
          ? 'Choose another issuer or clear the filter to review all keys.'
          : 'Create a scoped key after activating your first route.'}
      </p>
      {#if createdBy}<button
          class="button button-secondary"
          type="button"
          onclick={() => filterIssuer('')}>Clear issuer</button
        >{/if}
      {#if canManage && !createdBy}<a
          class="button button-primary"
          href={resolve('/api-keys/new')}>Create first key</a
        >{/if}
    </div>
  </section>
{:else}
  <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
  <div
    class="table-shell key-table"
    tabindex="0"
    role="region"
    aria-label="API keys"
  >
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
              ><a
                href={`${resolve('/api-keys')}?created_by=${key.created_by}`}
                aria-label={`Review API keys issued by ${key.created_by_email}`}
                >{key.created_by_email}</a
              ><br /><small>{formatDate(key.created_at)}</small></td
            >
            <td
              ><div class="row-actions">
                <a
                  class="button button-secondary"
                  href={resolve(`/usage?api_key_id=${key.id}`)}
                  aria-label={`Usage for ${key.name}`}>Usage</a
                >
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
  .issuer-filter {
    padding: 1rem;
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
