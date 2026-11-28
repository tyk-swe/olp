<script lang="ts">
  import FormField from '#lib/components/FormField.svelte';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { focusFormError } from '#lib/editing/focusError.js';
  import { hasChanges } from '#lib/editing/unsavedChanges.js';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { projectKeys } from '#lib/domain/projects/projectKeys.js';
  import {
    getBudget,
    putBudget,
    type AggregateBudget,
    type BudgetPolicy
  } from './api';
  import {
    aggregateBudgetDraft,
    aggregateBudgetPolicy,
    aggregateBudgetServerFields,
    validateAggregateBudget,
    type AggregateBudgetField
  } from './aggregateBudgetDraft';
  import { aggregateBudgetKeys } from './aggregateBudgetKeys';

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
  const basisId = $props.id();
  const budgetKey = $derived(
    aggregateBudgetKeys.detail(organizationId ?? projectId ?? 'installation')
  );
  const query = createQuery(() => ({
    queryKey: budgetKey,
    queryFn: ({ signal }) => getBudget(projectId, signal, organizationId)
  }));
  let draft = $state(aggregateBudgetDraft());
  let etag = $state('');
  let saved = $state.raw<BudgetPolicy | null>(null);
  const action = useAction<AggregateBudgetField>();
  function load(value: AggregateBudget) {
    draft = aggregateBudgetDraft(value.policy);
    etag = value.etag;
    saved = aggregateBudgetPolicy(draft);
  }
  $effect(() => {
    if (
      query.data &&
      (!etag ||
        (query.data.etag !== etag &&
          !hasChanges(saved, aggregateBudgetPolicy(draft))))
    )
      load(query.data);
  });
  async function reload() {
    const result = await query.refetch();
    if (result.data) {
      load(result.data);
      action.reset();
    }
  }
  async function submit(
    event: SubmitEvent & { currentTarget: HTMLFormElement }
  ) {
    event.preventDefault();
    const form = event.currentTarget;
    if (action.busy || !editable) return;
    const checked = validateAggregateBudget(draft);
    if (!action.check(checked)) return focusFormError(form);
    const accepted = await action.run(
      () => putBudget(projectId, etag, checked.value, organizationId),
      async (value) => {
        client.setQueryData(budgetKey, value);
        load(value);
        await client.invalidateQueries({ queryKey: projectKeys.root });
        notify(`${title} saved.`);
      },
      { serverFields: aggregateBudgetServerFields }
    );
    if (!accepted) await focusFormError(form);
  }
</script>

<section class="section" aria-label={title}>
  <h2>{title}</h2>
  <p class="section-help">
    Caps total inference spend, including shadow traffic and probes. Every
    applicable budget must allow a request. Subscription token allowances remain
    separate.
  </p>
  {#if query.isPending}
    <p role="status">Loading budget…</p>
  {:else if query.isError}
    <p class="inline-problem" role="alert">{errorMessage(query.error)}</p>
    <button class="button button-secondary" type="button" onclick={reload}>
      Retry budget
    </button>
  {:else}
    <form onsubmit={submit}>
      <fieldset disabled={action.busy || !editable}>
        <div class="form-field">
          <label for={basisId}>Budget basis</label>
          <select id={basisId} bind:value={draft.cost_basis}>
            <option value="cost">Provider cost</option>
            <option value="charge">Consumer charge</option>
          </select>
          <small>
            {#if (query.data?.usage as { cost_basis?: string }).cost_basis === 'charge'}
              Enforced on consumer charge.
            {:else}Enforced on provider cost.{/if}
          </small>
        </div>
        <FormField
          label="Daily cost limit (USD)"
          error={action.fieldErrors.daily_cost_limit}
        >
          {#snippet control(attributes)}
            <input
              {...attributes}
              inputmode="decimal"
              placeholder="Unlimited"
              bind:value={draft.daily_cost_limit}
            />
            <small>
              {query.data?.usage.daily.accrued ?? '0'} USD accrued this day.
            </small>
          {/snippet}
        </FormField>
        <FormField
          label="Weekly cost limit (USD)"
          error={action.fieldErrors.weekly_cost_limit}
        >
          {#snippet control(attributes)}
            <input
              {...attributes}
              inputmode="decimal"
              placeholder="Unlimited"
              bind:value={draft.weekly_cost_limit}
            />
            <small>
              {query.data?.usage.weekly?.accrued ?? '0'} USD accrued this ISO week.
            </small>
          {/snippet}
        </FormField>
        <FormField
          label="Monthly cost limit (USD)"
          error={action.fieldErrors.monthly_cost_limit}
        >
          {#snippet control(attributes)}
            <input
              {...attributes}
              inputmode="decimal"
              placeholder="Unlimited"
              bind:value={draft.monthly_cost_limit}
            />
            <small>
              {query.data?.usage.monthly.accrued ?? '0'} USD accrued this month.
            </small>
          {/snippet}
        </FormField>
      </fieldset>
      <p class="section-help">
        Blank limits remove the cap without resetting spend. Windows currently
        follow the installation budget time zone; weeks begin on Monday.
        Unpriced attempts accrue no USD; {query.data?.usage.unpriced_attempts ??
          0} this month.
      </p>
      {#if action.error}<p
          class="inline-problem"
          role="alert"
          tabindex="-1"
          data-error-summary
        >
          {action.error}
        </p>{/if}

      <div class="form-actions">
        {#if editable}<button
            class="button button-primary"
            type="submit"
            disabled={action.busy}
          >
            Save {title.toLowerCase()}
          </button>{/if}
        <button
          class="button button-secondary"
          type="button"
          disabled={action.busy}
          onclick={reload}
        >
          Reload budget
        </button>
      </div>
    </form>
  {/if}
</section>

<style>
  section {
    border-top: 1px solid var(--border);
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
