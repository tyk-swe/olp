<script lang="ts">
  import EntityPicker from '#lib/components/EntityPicker.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';

  import { currentBudgetLimit } from './budgetPresentation';
  import { useServiceCapabilities } from '#lib/features/access/session/serviceCapabilities.svelte.js';
  const services = useServiceCapabilities();
  import { apiKeyKeys } from '#lib/features/access/api-keys/apiKeyKeys.js';
  import { overviewKeys } from '#lib/features/overview/overviewKeys.js';

  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import {
    cursorPaginationProps,
    resetCursor
  } from '#lib/listing/pagination.js';

  import {
    listApiKeyPage,
    revokeApiKey,
    rotateApiKey,
    type ApiKey,
    type ApiKeySecret
  } from '#lib/features/access/api-keys/api.js';
  import SecretDialog from '#lib/shell/SecretDialog.svelte';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import NavIcon from '#lib/shell/NavIcon.svelte';
  import ReadOnlyNote from '#lib/shell/ReadOnlyNote.svelte';
  import { formatBudget, formatDate } from '#lib/format.js';
  import {
    budgetState,
    budgetStateLabel
  } from '#lib/features/access/api-keys/budgetPresentation.js';
  import type { ApiKeyListState } from '#lib/features/access/api-keys/apiKeyListState.js';
  import { isApiKeyExpired, isApiKeyUsable } from './apiKeyLifecycle';

  const revokeApiKeyIntent = useMutationIntent();

  let {
    listState = $bindable(),
    submitError,
    canManage,
    onSecret
  }: {
    listState: ApiKeyListState;
    submitError: string;
    canManage: boolean;
    onSecret: (secret: ApiKeySecret, preferredRoute?: string) => void;
  } = $props();

  const queryClient = useQueryClient();
  let busy = $state('');
  let rotating = $state<ApiKey | null>(null);
  let overlapSeconds = $state('0');
  let rotationError = $state('');
  let rotationId = $state('');
  let rotationAttempted = $state(false);
  let mutationError = $state('');
  const captureOwner = usePrincipalLifetime(() => {
    rotating = null;
    overlapSeconds = '0';
    rotationAttempted = false;
    rotationId = rotationError = mutationError = busy = '';
  });
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
    queryKey: apiKeyKeys.page(cursor, createdBy),
    queryFn: ({ signal }) => listApiKeyPage(cursor, signal, createdBy)
  }));

  async function filterIssuer(value: string) {
    const url = new URL(page.url.href);
    const id = value.trim().toLowerCase();
    if (id) url.searchParams.set('created_by', id);
    else url.searchParams.delete('created_by');
    const target = url.search
      ? resolve(`keys?${url.searchParams}${url.hash}`)
      : url.hash
        ? resolve(`keys#${url.hash.slice(1)}`)
        : resolve('keys');

    await goto(target, { reset: false });
  }

  async function rotate(key: ApiKey) {
    const isCurrent = captureOwner();

    if (!/^\d+$/.test(overlapSeconds) || Number(overlapSeconds) > 86400) {
      rotationError =
        'Overlap must be a whole number of seconds from 0 to 86400.';

      return;
    }
    busy = `rotate-${key.id}`;
    rotationAttempted = true;
    mutationError = '';
    try {
      const replacement = await rotateApiKey(
        key,
        Number(overlapSeconds),
        rotationId
      );
      if (!isCurrent()) return;
      rotating = null;
      onSecret(replacement, key.allowed_routes[0]);
      await queryClient.invalidateQueries({ queryKey: apiKeyKeys.root });
    } catch (error) {
      if (isCurrent()) {
        rotationError = errorMessage(error);
        mutationError = rotationError;
      }
    } finally {
      if (isCurrent()) busy = '';
    }
  }

  async function revoke(key: ApiKey) {
    if (
      !(await confirmAction({
        title: `Revoke ${key.name}?`,
        body: 'Requests using this key start failing right away. You can’t undo this.',
        confirmLabel: 'Revoke key',
        danger: true
      }))
    )
      return;
    const isCurrent = captureOwner();
    busy = `revoke-${key.id}`;
    mutationError = '';
    try {
      await revokeApiKeyIntent.submit(
        { id: key.id, etag: key.etag },
        (input, requestId) => revokeApiKey(input, requestId)
      );
      if (!isCurrent()) return;
      await queryClient.invalidateQueries({ queryKey: apiKeyKeys.root });
      await queryClient.invalidateQueries({ queryKey: overviewKeys.root });
    } catch (error) {
      if (isCurrent()) mutationError = errorMessage(error);
    } finally {
      if (isCurrent()) busy = '';
    }
  }
</script>

<PageHeader
  title="API keys"
  description="Manage client access with scoped API keys, route permissions, and usage limits."
>
  {#snippet actions()}
    {#if canManage}<a class="button button-primary" href={resolve('keys/new')}
        >New API key <NavIcon name="arrow" /></a
      >
    {/if}
  {/snippet}
</PageHeader>
{#if !services.limitsEnforced}<p class="muted" role="status">
    Limit and budget policies are saved but not enforced yet.
  </p>{/if}
{#if !canManage}
  <ReadOnlyNote>
    Your role can view API keys but not create, edit, rotate, or revoke them.
  </ReadOnlyNote>
{/if}
{#if submitError || mutationError}<div class="inline-problem" role="alert">
    {submitError || mutationError}
  </div>{/if}

<div class="toolbar issuer-filter">
  <label for="key-issuer"
    >Created by <EntityPicker
      id="key-issuer"
      kind="person"
      bind:value={issuer}
      emptyLabel="Anyone"
      onchange={filterIssuer}
    /></label
  >
</div>

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
      <h2>
        {createdBy ? 'No API keys created by this person' : 'No API keys'}
      </h2>
      <p>
        {createdBy
          ? 'Pick someone else or show every key.'
          : 'Create a scoped key for your clients.'}
      </p>
      {#if createdBy}<button
          class="button button-secondary"
          type="button"
          onclick={() => filterIssuer('')}>Show all keys</button
        >{/if}
      {#if canManage && !createdBy}<a
          class="button button-primary"
          href={resolve('keys/new')}>Create first key</a
        >
      {/if}
    </div>
  </section>
{:else}
  <!-- svelte-ignore a11y_no_noninteractive_tabindex (Keyboard focus enables scrolling the table.) -->
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
          {@const budget = budgetState(key.budget)}
          {@const rates = key.effective_limits ?? key}
          {@const expired = isApiKeyExpired(key)}
          {@const usable = isApiKeyUsable(key)}
          <tr>
            <td
              ><strong>{key.name}</strong><br /><code>{key.lookup_id}</code><br
              /><small>{key.project_name ?? 'Installation-wide'}</small></td
            >
            <td
              ><span
                class:danger={Boolean(key.revoked_at)}
                class:warning={expired}
                class:success={usable}
                class="badge"
                >{key.revoked_at
                  ? 'revoked'
                  : expired
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
              >
              {#if key.rotation_due_at}<br /><small
                  >Rotation due {formatDate(key.rotation_due_at)}</small
                >{/if}
              {#each key.active_overlaps ?? [] as overlap (overlap.lookup_id)}<br
                /><small
                  >Previous {overlap.lookup_id} valid until {formatDate(
                    overlap.expires_at
                  )}</small
                >{/each}
            </td>
            <td
              >{key.scopes.join(', ') || 'none'}<br />
              {#if key.allowed_routes.length}<small
                  >{key.allowed_routes.join(', ')}</small
                >{/if}
              {#if key.allowed_route_groups?.length}{#if key.allowed_routes.length}<br
                  />{/if}<small
                  >Groups: {key.allowed_route_groups.join(', ')}</small
                >
              {:else if !key.allowed_routes.length}<small>all routes</small
                >{/if}
            </td>
            <td
              ><small
                >{rates.requests_per_minute
                  ? `${rates.requests_per_minute} RPM`
                  : 'unlimited RPM'}<br />{rates.tokens_per_minute
                  ? `${rates.tokens_per_minute} TPM`
                  : 'unlimited TPM'} · {rates.max_concurrency
                  ? `${rates.max_concurrency} concurrent`
                  : 'unlimited concurrency'}</small
              ></td
            >
            <td>
              {#if !services.limitsEnforced || budget === 'policy'}<small
                  >Saved policy · not enforced</small
                >
              {:else if currentBudgetLimit(key.budget.daily) !== null || currentBudgetLimit(key.budget.monthly) !== null || currentBudgetLimit(key.budget.weekly) != null}
                <small>
                  {#if currentBudgetLimit(key.budget.daily) !== null}Daily {formatBudget(
                      key.budget.daily.accrued
                    )} / {formatBudget(
                      currentBudgetLimit(key.budget.daily)
                    )}{/if}
                  {#if key.budget.weekly && currentBudgetLimit(key.budget.weekly) != null}{#if currentBudgetLimit(key.budget.daily) !== null}<br
                      />{/if}Weekly {formatBudget(key.budget.weekly.accrued)} / {formatBudget(
                      currentBudgetLimit(key.budget.weekly)
                    )}{/if}
                  {#if (currentBudgetLimit(key.budget.daily) !== null || currentBudgetLimit(key.budget.weekly) != null) && currentBudgetLimit(key.budget.monthly) !== null}<br
                    />{/if}
                  {#if currentBudgetLimit(key.budget.monthly) !== null}Monthly {formatBudget(
                      key.budget.monthly.accrued
                    )} / {formatBudget(
                      currentBudgetLimit(key.budget.monthly)
                    )}{/if}
                </small>
                {#if budget === 'exhausted' || budget === 'unknown'}<br /><span
                    class="badge"
                    class:danger={budget === 'exhausted'}
                    class:warning={budget === 'unknown'}
                    >{budgetStateLabel(budget)}</span
                  >{/if}
              {:else}
                <small>No cost budget</small>
              {/if}
            </td>
            <td>
              <a
                href={`${resolve('keys')}?created_by=${key.created_by}`}
                aria-label={`Review API keys issued by ${key.created_by_email}`}
                >{key.created_by_email}</a
              ><br /><small>{formatDate(key.created_at)}</small></td
            >
            <td
              ><div class="row-actions">
                {#if services.gatewayAvailable}<a
                    class="button button-secondary"
                    href={resolve(`usage?api_key_id=${key.id}`)}
                    aria-label={`Usage for ${key.name}`}>Usage</a
                  >
                {/if}

                <a
                  class="button button-secondary"
                  href={resolve(`keys/${key.id}`)}
                  >{canManage && usable && !key.workload_issuer_id
                    ? 'Edit'
                    : 'View'}</a
                >{#if canManage && !key.revoked_at}{#if usable && !key.workload_issuer_id}<button
                      class="button button-secondary"
                      type="button"
                      onclick={() => {
                        rotating = key;
                        overlapSeconds = '0';
                        rotationAttempted = false;
                        rotationId = crypto.randomUUID();
                        rotationError = '';
                      }}
                      disabled={Boolean(busy)}
                      >{busy === `rotate-${key.id}`
                        ? 'Rotating…'
                        : 'Rotate'}</button
                    >{/if}<button
                    class="button button-secondary button-danger"
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

{#if rotating}
  <SecretDialog
    eyebrow="Explicit rotation"
    title={`Rotate ${rotating.name}`}
    description="Generate a new secret and choose how long the previous secret remains valid. All versions share the key's current permissions, limits and budgets."
    onClose={() => {
      rotating = null;
    }}
  >
    {#snippet children(close)}
      <form
        onsubmit={(event) => {
          event.preventDefault();
          if (rotating) void rotate(rotating);
        }}
      >
        <div class="form-field full">
          <label for="rotation-overlap">Previous-secret overlap (seconds)</label
          ><input
            id="rotation-overlap"
            data-autofocus
            inputmode="numeric"
            bind:value={overlapSeconds}
            disabled={Boolean(busy) || rotationAttempted}
            aria-describedby="rotation-overlap-help"
          /><small id="rotation-overlap-help"
            >0 ends all previous overlaps when gateways refresh. A positive
            value allows up to 24 hours, bounded by key expiry; existing overlap
            deadlines stay unchanged.</small
          >
        </div>
        {#if rotationError}<div class="inline-problem" role="alert">
            {rotationError} Retry uses the same request and overlap to recover its
            result. Close this dialog to start a new rotation.
          </div>{/if}
        <div class="form-actions">
          <button
            class="button button-primary"
            type="submit"
            disabled={Boolean(busy)}>Rotate key</button
          ><button
            class="button button-secondary"
            type="button"
            onclick={close}
            disabled={Boolean(busy)}>Cancel</button
          >
        </div>
      </form>
    {/snippet}
  </SecretDialog>
{/if}

<style>
  .issuer-filter {
    margin-top: 1.5rem;
  }
  .issuer-filter label {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .issuer-filter :global(:is(select, input)) {
    min-width: min(18rem, 60vw);
  }
  .key-table {
    margin-top: 1.5rem;
  }
  code {
    font-size: var(--text-caption);
  }
  td small {
    color: var(--foreground-muted);
  }
  .row-actions {
    display: flex;
    gap: 0.4rem;
  }
</style>
