<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { formatDate } from '$lib/format';
  import { useRole } from '../session/useRole.svelte';
  import {
    createIncrease,
    listIncreases,
    revokeIncrease,
    type BudgetIncrease,
    type IncreaseInput
  } from './api';
  import { limitsError, limitForm } from '../budgets/limitForm';
  const access = useRole();
  const client = useQueryClient();
  const key = ['budget-increases'];
  let before = $state<string | undefined>();
  const query = createQuery(() => ({
    queryKey: [...key, before],
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
  let notice = $state('');
  let submitted = $state<IncreaseInput | null>(null);
  let requestId = crypto.randomUUID();
  function reset() {
    submitted = null;
    requestId = crypto.randomUUID();
    problem = '';
  }
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (busy || !canCreate) return;
    problem = '';
    notice = '';
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
      notice = `Increase recorded until ${formatDate(result.expires_at)}.`;
      reset();
      await client.invalidateQueries({ queryKey: key });
    } catch (error) {
      problem = errorMessage(error);
    } finally {
      busy = false;
    }
  }
  async function revoke(item: BudgetIncrease) {
    if (busy) return;
    busy = true;
    problem = '';
    notice = '';
    try {
      await revokeIncrease(item);
      notice = 'Increase revoked.';
      await client.invalidateQueries({ queryKey: key });
    } catch (error) {
      problem = errorMessage(error);
    } finally {
      busy = false;
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
<div class="page-header">
  <div>
    <p class="eyebrow">Access</p>
    <h1 class="page-title">Budget increases</h1>
    <p class="page-description">
      Raise one existing cost cap temporarily, with a recorded reason and
      automatic expiry.
    </p>
  </div>
</div>
<p>
  Increases add to the current permanent cap and share its accrued and in-flight
  spending. Every other budget still applies. An increase ends at its expiry or
  the current calendar-window boundary, whichever comes first.
</p>
{#if problem}<p role="alert" class="inline-problem">{problem}</p>{/if}
{#if notice}<p role="status">{notice}</p>{/if}
{#if canCreate}
  <form onsubmit={submit}>
    <fieldset disabled={busy || submitted !== null}>
      <legend>New increase</legend>
      <div class="fields">
        <div class="form-field">
          <label for="increase-kind">Budget boundary</label><select
            id="increase-kind"
            bind:value={kind}
            >{#each kinds as option (option.value)}<option value={option.value}
                >{option.label}</option
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
        Retry preserves the original request to recover a lost response without
        adding another increase.
      </p>{/if}
  </form>
{/if}
{#if query.isPending}<p role="status">
    Loading increases…
  </p>{:else if query.isError}<p role="alert" class="inline-problem">
    {errorMessage(query.error)}
  </p>
  <button
    type="button"
    class="button button-secondary"
    onclick={() => query.refetch()}>Retry list</button
  >{:else}
  {#each query.data?.items ?? [] as item (item.id)}
    <article>
      <h2>{item.target.kind.replaceAll('_', ' ')} · {item.window_kind}</h2>
      <p>+${item.amount} until {formatDate(item.expires_at)}</p>
      <p>{item.reason}</p>
      <dl>
        <dt>Resource</dt>
        <dd>{item.target.id ?? 'Installation'} {item.target.route ?? ''}</dd>
        {#if item.target.end_user_digest}<dt>End-user digest</dt>
          <dd>{item.target.end_user_digest}</dd>{/if}{#if item.target.label}<dt>
            Attribution
          </dt>
          <dd>{item.target.label}={item.target.value}</dd>{/if}
        <dt>Created</dt>
        <dd>{formatDate(item.created_at)} · {item.created_by}</dd>
      </dl>
      {#if item.revoked_at}<p>
          Revoked {formatDate(item.revoked_at)}
        </p>{:else if new Date(item.expires_at).getTime() <= Date.now()}<p>
          Expired
        </p>{:else if canCreate && (item.target.kind !== 'installation' || canInstall)}<button
          type="button"
          class="button button-secondary"
          disabled={busy}
          onclick={() => revoke(item)}>Revoke increase</button
        >{/if}
    </article>
  {:else}<p>No increases in this page.</p>{/each}
  <div>
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
        }}>Older increases</button
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
  article {
    border-top: 1px solid var(--border);
    margin-top: 1.5rem;
    padding-top: 1rem;
  }
  dd {
    overflow-wrap: anywhere;
  }
</style>
