<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { formatDate } from '#lib/format.js';
  import { useRole } from '../session/useRole.svelte';
  import {
    createIncrease,
    listIncreases,
    revokeIncrease,
    type BudgetIncrease,
    type IncreaseInput
  } from './api';
  import { limitsError, limitForm } from '../budgets/limitForm';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { budgetIncreaseKeys } from './budgetIncreaseKeys';
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import StatusBadge from '#lib/components/StatusBadge.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  const access = useRole();
  const client = useQueryClient();
  let before = $state<string | undefined>();
  const query = createQuery(() => ({
    queryKey: budgetIncreaseKeys.page(before),
    queryFn: ({ signal }) => listIncreases(before, signal)
  }));
  const canCreate = $derived(access.allows('POST /api/v1/budget-increases'));
  const canInstall = $derived(
    access.allows('PUT /api/v1/budgets/installation')
  );
  // The server also requires the caller to manage the named organization.
  const canOrganize = $derived(
    access.allows('PATCH /api/v1/organizations/{organization_id}')
  );
  let kind = $state<IncreaseInput['target']['kind']>('api_key');
  let resourceId = $state('');
  let route = $state('');
  let digest = $state('');
  let label = $state('');
  let value = $state('');
  let window = $state<IncreaseInput['window']>('day');
  let amount = $state('');
  let reason = $state('');
  let expires = $state('');
  let busy = $state(false);
  let problem = $state('');
  let creating = $state(false);
  let submitted = $state<IncreaseInput | null>(null);
  let requestId = crypto.randomUUID();
  const captureOwner = usePrincipalLifetime(() => {
    reset();
    resourceId =
      route =
      digest =
      label =
      value =
      amount =
      reason =
      expires =
        '';
    kind = 'api_key';
    window = 'day';
    before = undefined;
    busy = false;
  });
  function reset() {
    submitted = null;
    requestId = crypto.randomUUID();
    problem = '';
  }
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (busy || !canCreate) return;
    const isCurrent = captureOwner();
    let committed = false;
    problem = '';
    if (!submitted) {
      problem = limitsError({ ...limitForm(), daily_cost_limit: amount });
      if (!amount.trim() || !reason.trim())
        problem = 'Supply a positive USD amount and a reason.';
      if (expires && Number.isNaN(new Date(expires).getTime()))
        problem = 'Choose a valid expiry.';
      if (problem) return;
      const target: IncreaseInput['target'] = { kind };
      if (kind !== 'installation') target.id = resourceId.trim();
      if (kind === 'key_route') target.route = route.trim();
      if (kind === 'key_end_user' || kind === 'project_end_user')
        target.end_user_digest = digest.trim();
      if (kind === 'attribution') {
        target.label = label.trim();
        target.value = value.trim();
      }
      submitted = {
        target,
        window,
        amount: amount.trim(),
        reason: reason.trim(),
        expires_at: expires ? new Date(expires).toISOString() : null
      };
    }
    busy = true;
    try {
      const result = await createIncrease(submitted, requestId);
      if (!isCurrent()) return;
      committed = true;
      creating = false;
      notify(`Increase granted until ${formatDate(result.expires_at)}`);
      reset();
      await client.invalidateQueries(
        { queryKey: budgetIncreaseKeys.root },
        { throwOnError: true }
      );
    } catch (error) {
      if (isCurrent())
        problem =
          (committed
            ? 'Increase recorded, but refreshing the view failed: '
            : '') + errorMessage(error);
    } finally {
      if (isCurrent()) busy = false;
    }
  }
  async function revoke(item: BudgetIncrease) {
    if (busy) return;
    const isCurrent = captureOwner();
    let committed = false;
    busy = true;
    problem = '';
    try {
      await revokeIncrease(item);
      if (!isCurrent()) return;
      committed = true;
      notify('Increase revoked');
      await client.invalidateQueries(
        { queryKey: budgetIncreaseKeys.root },
        { throwOnError: true }
      );
    } catch (error) {
      if (isCurrent())
        problem =
          (committed
            ? 'Increase revoked, but refreshing the view failed: '
            : '') + errorMessage(error);
    } finally {
      if (isCurrent()) busy = false;
    }
  }
  const kinds: { value: IncreaseInput['target']['kind']; label: string }[] = [
    { value: 'api_key', label: 'API key' },
    { value: 'budget_group', label: 'Budget group' },
    { value: 'project', label: 'Project' },
    { value: 'key_route', label: 'API key and route' },
    { value: 'key_end_user', label: 'API key and end user' },
    { value: 'project_end_user', label: 'Project and end user' },
    { value: 'attribution', label: 'Project attribution' }
  ];
</script>

<svelte:head><title>Budget increases · OpenLLMProxy</title></svelte:head>
<div class="toolbar">
  <p class="lede">
    A temporary increase raises a budget cap until it expires or the budget
    window ends. Every other budget still applies.
  </p>
  {#if canCreate}<div class="page-actions">
      <button
        class="button button-primary"
        type="button"
        onclick={() => {
          problem = '';
          creating = true;
        }}>New increase</button
      >
    </div>{/if}
</div>
{#if problem && !creating}<Callout tone="danger">{problem}</Callout>{/if}
{#if canCreate && creating}
  <Dialog
    title="New budget increase"
    placement="side"
    onClose={() => (creating = false)}
  >
    {#if problem}<Callout tone="danger">{problem}</Callout>{/if}
    <form onsubmit={submit}>
      <fieldset disabled={busy || submitted !== null}>
        <div class="fields">
          <div class="form-field">
            <label for="increase-kind">Budget boundary</label><select
              id="increase-kind"
              bind:value={kind}
              >{#each kinds as option (option.value)}<option
                  value={option.value}>{option.label}</option
                >{/each}{#if canOrganize}<option value="organization"
                  >Organization</option
                >{/if}{#if canInstall}<option value="installation"
                  >Installation</option
                >{/if}</select
            >
          </div>
          {#if kind !== 'installation'}<div class="form-field">
              <label for="increase-resource"
                >{kind === 'budget_group'
                  ? 'Budget group'
                  : kind === 'organization'
                    ? 'Organization'
                    : kind === 'project' ||
                        kind === 'project_end_user' ||
                        kind === 'attribution'
                      ? 'Project'
                      : 'API key'} ID</label
              ><input
                id="increase-resource"
                bind:value={resourceId}
                required
                placeholder="Resource UUID"
              />
            </div>{/if}
          {#if kind === 'key_route'}<div class="form-field">
              <label for="increase-route">Route slug</label><input
                id="increase-route"
                bind:value={route}
                required
              />
            </div>{/if}
          {#if kind === 'key_end_user' || kind === 'project_end_user'}<div
              class="form-field"
            >
              <label for="increase-digest">End-user digest</label><input
                id="increase-digest"
                bind:value={digest}
                required
                pattern={'[0-9a-f]{64}'}
              /><small
                >Use the digest from end-user lookup, never a raw identifier.</small
              >
            </div>{/if}
          {#if kind === 'attribution'}<div class="form-field">
              <label for="increase-label">Attribution label</label><input
                id="increase-label"
                bind:value={label}
                required
              />
            </div>
            <div class="form-field">
              <label for="increase-value">Attribution value</label><input
                id="increase-value"
                bind:value
                required
              />
            </div>{/if}
          <div class="form-field">
            <label for="increase-window">Budget window</label><select
              id="increase-window"
              bind:value={window}
              ><option value="day">Day</option><option value="week">Week</option
              ><option value="month">Month</option></select
            >
          </div>
          <div class="form-field">
            <label for="increase-amount">Additional USD</label><input
              id="increase-amount"
              bind:value={amount}
              inputmode="decimal"
              required
            />
          </div>
          <div class="form-field">
            <label for="increase-expiry">Earlier expiry (optional)</label><input
              id="increase-expiry"
              type="datetime-local"
              bind:value={expires}
            /><small>Your local time. Blank uses the budget-window end.</small>
          </div>
          <div class="form-field">
            <label for="increase-reason">Reason</label><input
              id="increase-reason"
              bind:value={reason}
              required
              maxlength="256"
            />
          </div>
        </div>
      </fieldset>
      <button class="button button-primary" disabled={busy} type="submit"
        >{submitted ? 'Retry increase' : 'Create increase'}</button
      >
      {#if submitted}<button
          class="button button-secondary"
          type="button"
          disabled={busy}
          onclick={reset}>Start another request</button
        >
        <p>
          Retry preserves the original request to recover a lost response
          without adding another increase.
        </p>{/if}
    </form>
  </Dialog>
{/if}
{#if query.isPending}<p class="loading-state" role="status">
    Loading increases…
  </p>{:else if query.isError}<Callout
    tone="danger"
    title="Increases could not be loaded"
    >{errorMessage(query.error)}{#snippet actions()}<button
        type="button"
        class="button button-secondary"
        onclick={() => query.refetch()}>Retry</button
      >{/snippet}</Callout
  >{:else if !query.data?.items.length && !before}<EmptyState
    title="No budget increases"
    >Increases you grant show up here until they expire.</EmptyState
  >{:else}
  <ScrollTable label="Budget increases">
    <table class="data-table">
      <thead
        ><tr
          ><th scope="col">Applies to</th><th scope="col">Amount</th><th
            scope="col">Reason</th
          ><th scope="col">Status</th><th scope="col">Granted</th><th
            scope="col"><span class="sr-only">Actions</span></th
          ></tr
        ></thead
      >
      <tbody>
        {#each query.data?.items ?? [] as item (item.id)}
          {@const expired = new Date(item.expires_at).getTime() <= Date.now()}
          <tr>
            <td>
              <strong
                >{kinds.find((choice) => choice.value === item.target.kind)
                  ?.label ?? item.target.kind}</strong
              >
              <span class="muted">{item.window_kind} window</span>
              {#if item.target.id}<code class="mono">{item.target.id}</code
                >{/if}{#if item.target.route}<code class="mono"
                  >{item.target.route}</code
                >{/if}{#if item.target.label}<code class="mono"
                  >{item.target.label}={item.target.value}</code
                >{/if}
            </td>
            <td class="mono">+${item.amount}</td>
            <td>{item.reason}</td>
            <td
              >{#if item.revoked_at}<StatusBadge
                  label="Revoked"
                  tone="neutral"
                />{:else if expired}<StatusBadge
                  label="Expired"
                  tone="neutral"
                />{:else}<StatusBadge
                  label={`Until ${formatDate(item.expires_at)}`}
                  tone="success"
                />{/if}</td
            >
            <td
              >{formatDate(item.created_at)}<span class="muted"
                >{item.created_by}</span
              ></td
            >
            <td
              >{#if !item.revoked_at && !expired && canCreate && (item.target.kind !== 'installation' || canInstall)}<button
                  type="button"
                  class="text-button danger"
                  disabled={busy}
                  onclick={() => revoke(item)}>Revoke</button
                >{/if}</td
            >
          </tr>
        {/each}
      </tbody>
    </table>
  </ScrollTable>
  <div class="list-pager">
    {#if before}<button
        type="button"
        class="button button-secondary"
        onclick={() => {
          before = undefined;
        }}>Newest</button
      >{/if}{#if query.data?.next_cursor}<button
        type="button"
        class="button button-secondary"
        onclick={() => {
          before = query.data?.next_cursor ?? undefined;
        }}>Older</button
      >{/if}
  </div>
{/if}

<style>
  .fields {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 18rem), 1fr));
    gap: 1rem;
  }
  fieldset {
    border: 0;
    padding: 0;
    margin: 1rem 0;
  }
  .lede {
    max-width: 60ch;
    margin: 0;
    color: var(--foreground-muted);
  }
  td code,
  .muted {
    display: block;
    overflow-wrap: anywhere;
  }
  .muted {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
</style>
