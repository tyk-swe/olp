<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import PolicyFields from '../attribution-budgets/BudgetFields.svelte';
  import {
    budgetError,
    budgetForm,
    budgetInput
  } from '../attribution-budgets/policy';
  import {
    getProjectAttributionBudgets,
    putProjectAttributionBudgets,
    type ProjectAttributionBudgets
  } from './api';
  import { projectKeys } from './projectKeys';

  let {
    projectId,
    editable = true
  }: { projectId: string; editable?: boolean } = $props();
  const client = useQueryClient();
  const policy = createQuery(() => ({
    queryKey: [...projectKeys.root, 'attribution-budgets', projectId],
    queryFn: ({ signal }) => getProjectAttributionBudgets(projectId, signal)
  }));
  let form = $state(budgetForm());
  let etag = $state('');
  let loaded = $state('');
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');

  function load(value: ProjectAttributionBudgets) {
    form = budgetForm(value.budgets);
    etag = value.etag;
    loaded = JSON.stringify(budgetInput(form));
  }
  $effect(() => {
    if (
      policy.data &&
      (!etag ||
        (policy.data.etag !== etag &&
          JSON.stringify(budgetInput(form)) === loaded))
    )
      load(policy.data);
  });

  async function reload() {
    const result = await policy.refetch();
    if (result.data) {
      load(result.data);
      error = notice = '';
    }
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (busy || !editable) return;
    error = budgetError(form);
    notice = '';
    if (error) return;
    busy = true;
    try {
      const saved = await putProjectAttributionBudgets(
        projectId,
        etag,
        budgetInput(form)
      );
      client.setQueryData(
        [...projectKeys.root, 'attribution-budgets', projectId],
        saved
      );
      load(saved);
      await client.invalidateQueries({ queryKey: projectKeys.root });
      notice = 'Attribution budgets saved.';
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
</script>

<section aria-label="Project attribution budgets">
  {#if policy.isPending}
    <p role="status">Loading attribution budgets…</p>
  {:else if policy.isError}
    <p class="inline-problem" role="alert">{errorMessage(policy.error)}</p>
    <button class="button button-secondary" type="button" onclick={reload}
      >Retry attribution budgets</button
    >
  {:else if etag}
    <form onsubmit={submit}>
      <p class="muted">
        All matching pairs are enforced alongside other budgets. Clearing a cap
        preserves current spending. Calendar windows follow the installation
        time zone.
      </p>
      <PolicyFields bind:form disabled={busy || !editable} />
      {#if policy.data && Object.keys(policy.data.usage).length}
        <details>
          <summary>Current attributed spending</summary>
          <ul>
            {#each Object.entries(policy.data.usage) as [label, values] (label)}
              {#each Object.entries(values) as [value, usage] (value)}
                <li>
                  {label}={value}: ${usage.daily.accrued} today, ${usage.weekly
                    ?.accrued ?? '0'} this week, ${usage.monthly.accrued} this month.
                  {usage.unpriced_attempts} unpriced attempts this month.
                </li>
              {/each}
            {/each}
          </ul>
        </details>
      {/if}
      {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
      {#if notice}<p class="notice" role="status">{notice}</p>{/if}
      <div class="actions">
        {#if editable}<button
            class="button button-primary"
            type="submit"
            disabled={busy}
            >{busy ? 'Saving…' : 'Save attribution budgets'}</button
          >{/if}
        <button
          type="button"
          class="button button-secondary"
          disabled={busy}
          onclick={reload}>Reload saved policy</button
        >
      </div>
    </form>
  {/if}
</section>

<style>
  section {
    margin: 1.5rem 0;
    padding: 1rem 0;
    border-top: 1px solid var(--border-hairline);
    border-bottom: 1px solid var(--border-hairline);
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.75rem;
  }
</style>
