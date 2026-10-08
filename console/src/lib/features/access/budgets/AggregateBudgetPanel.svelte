<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { projectKeys } from '../projects/projectKeys';
  import {
    getBudget,
    putBudget,
    type AggregateBudget,
    type BudgetPolicy
  } from './api';

  let {
    projectId,
    organizationId,
    editable = true
  }: {
    organizationId?: string;
    projectId?: string;
    editable?: boolean;
  } = $props();
  const client = useQueryClient();
  const title = $derived(
    organizationId
      ? 'Organization budget'
      : projectId
        ? 'Project budget'
        : 'Installation budget'
  );
  const prefix = $derived(
    `aggregate-${organizationId ?? projectId ?? 'installation'}`
  );
  const budgetKey = $derived([
    ...projectKeys.root,
    'aggregate-budget',
    organizationId ?? projectId ?? 'installation'
  ]);
  const query = createQuery(() => ({
    queryKey: budgetKey,
    queryFn: ({ signal }) => getBudget(projectId, signal, organizationId)
  }));
  let daily = $state('');
  let monthly = $state('');
  let weekly = $state('');
  let etag = $state('');
  let saved = $state('');
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');
  function input(): BudgetPolicy | null {
    return daily || weekly || monthly
      ? {
          daily_cost_limit: daily || null,
          monthly_cost_limit: monthly || null,
          weekly_cost_limit: weekly || null
        }
      : null;
  }
  function load(value: AggregateBudget) {
    daily = value.policy?.daily_cost_limit ?? '';
    monthly = value.policy?.monthly_cost_limit ?? '';
    weekly = value.policy?.weekly_cost_limit ?? '';
    etag = value.etag;
    saved = JSON.stringify(input());
  }
  $effect(() => {
    if (
      query.data &&
      (!etag || (query.data.etag !== etag && JSON.stringify(input()) === saved))
    )
      load(query.data);
  });
  async function reload() {
    const result = await query.refetch();
    if (result.data) {
      load(result.data);
      error = notice = '';
    }
  }
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (busy || !editable) return;
    error = notice = '';
    if (
      [daily, weekly, monthly].some(
        (value) =>
          value &&
          (!/^\d{1,12}(\.\d{1,12})?$/.test(value) || Number(value) <= 0)
      )
    ) {
      error =
        'Budgets must be positive USD amounts with at most 12 digits before and after the decimal point.';
      return;
    }
    busy = true;
    try {
      const value = await putBudget(projectId, etag, input(), organizationId);
      client.setQueryData(budgetKey, value);
      load(value);
      await client.invalidateQueries({ queryKey: projectKeys.root });
      notice = `${title} saved.`;
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
</script>

<section aria-label={title}>
  <h2>{title}</h2>
  <p>
    Caps total inference spend, including shadow traffic and probes. Every
    applicable budget must allow a request. Subscription token allowances remain
    separate.
  </p>
  {#if query.isPending}
    <p role="status">Loading budget…</p>
  {:else if query.isError}
    <p class="inline-problem" role="alert">{errorMessage(query.error)}</p>
    <button class="button button-secondary" type="button" onclick={reload}
      >Retry budget</button
    >
  {:else}
    <form onsubmit={submit}>
      <fieldset disabled={busy || !editable}>
        <div class="form-field">
          <label for={`${prefix}-daily`}>Daily cost limit (USD)</label>
          <input
            id={`${prefix}-daily`}
            inputmode="decimal"
            placeholder="Unlimited"
            bind:value={daily}
          />
          <small
            >{query.data?.usage.daily.accrued ?? '0'} USD accrued this day.</small
          >
        </div>
        <div class="form-field">
          <label for={`${prefix}-weekly`}>Weekly cost limit (USD)</label>
          <input
            id={`${prefix}-weekly`}
            inputmode="decimal"
            placeholder="Unlimited"
            bind:value={weekly}
          />
          <small
            >{query.data?.usage.weekly?.accrued ?? '0'} USD accrued this ISO week.</small
          >
        </div>
        <div class="form-field">
          <label for={`${prefix}-monthly`}>Monthly cost limit (USD)</label>
          <input
            id={`${prefix}-monthly`}
            inputmode="decimal"
            placeholder="Unlimited"
            bind:value={monthly}
          />
          <small
            >{query.data?.usage.monthly.accrued ?? '0'} USD accrued this month.</small
          >
        </div>
      </fieldset>
      <p>
        Blank limits remove the cap without resetting spend. Windows currently
        follow the installation budget time zone; weeks begin on Monday.
        Unpriced attempts accrue no USD; {query.data?.usage.unpriced_attempts ??
          0} this month.
      </p>
      {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
      {#if notice}<p role="status">{notice}</p>{/if}
      <div class="form-actions">
        {#if editable}<button
            class="button button-primary"
            type="submit"
            disabled={busy}>Save {title.toLowerCase()}</button
          >{/if}
        <button
          class="button button-secondary"
          type="button"
          disabled={busy}
          onclick={reload}>Reload budget</button
        >
      </div>
    </form>
  {/if}
</section>

<style>
  section {
    border-top: 1px solid var(--border);
    margin-top: 1.5rem;
    padding-top: 1.5rem;
  }
  fieldset {
    border: 0;
    padding: 0;
    margin: 0;
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 15rem), 1fr));
    gap: 1rem;
  }
</style>
