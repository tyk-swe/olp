<script lang="ts">
  import type { ApiKey } from './api.js';
  import type { ApiKeyFormState } from './apiKeyPolicy.js';
  import type { useServiceCapabilities } from '#lib/session/serviceCapabilities.svelte.js';
  import type { listBudgetGroups } from '#lib/features/budgets/index.js';
  import {
    budgetStateNote,
    budgetWindowState,
    currentBudgetLimit
  } from './budgetPresentation.js';
  import { formatBudget, formatDate, formatInteger } from '#lib/format.js';

  let {
    form,
    errors,
    canManage,
    editing,
    services,
    groups,
    matchingGroups
  }: {
    form: ApiKeyFormState;
    errors: Record<string, string>;
    canManage: boolean;
    editing: ApiKey | null;
    services: ReturnType<typeof useServiceCapabilities>;
    groups: {
      isPending: boolean;
      data?: Awaited<ReturnType<typeof listBudgetGroups>>;
    };
    matchingGroups: Awaited<ReturnType<typeof listBudgetGroups>>;
  } = $props();
</script>

<section aria-labelledby="budget-heading">
  <h2 id="budget-heading">Spending limits</h2>
  <p class="section-help">
    {#if services.limitsEnforced}Amounts use the installation currency and time
      zone. Requests are refused once the spend so far plus requests still
      running would pass the limit. These are estimates, so treat them as guard
      rails rather than invoice caps.{:else}Save daily and monthly budget
      amounts for future enforcement. Spending is not measured or restricted
      yet.{/if}
  </p>
  <div class="form-grid budget-inputs">
    <div class="form-field">
      <label for="budget-group">Shared budget group (optional)</label
      >{#if groups.isPending}<span class="inline-status" role="status"
          >Loading groups…</span
        >{:else}<select
          id="budget-group"
          bind:value={form.budgetGroupId}
          disabled={!canManage}
        >
          <option value="">No shared group</option>
          {#each matchingGroups as group (group.id)}<option value={group.id}
              >{group.name}</option
            >{/each}
        </select><small
          >All keys in the group share its cost budget and keep their own
          limits.</small
        >{/if}
    </div>
    <div class="form-field">
      <label for="daily-budget">Daily cost budget (optional)</label><input
        id="daily-budget"
        inputmode="decimal"
        placeholder="10.00"
        bind:value={form.dailyCostLimit}
        disabled={!canManage}
        aria-invalid={errors.dailyCostLimit ? 'true' : undefined}
        aria-describedby={errors.dailyCostLimit
          ? 'daily-budget-error'
          : undefined}
      />{#if errors.dailyCostLimit}<small
          class="field-error"
          id="daily-budget-error">{errors.dailyCostLimit}</small
        >{/if}
    </div>
    <div class="form-field">
      <label for="weekly-budget">Weekly cost budget (optional)</label><input
        id="weekly-budget"
        inputmode="decimal"
        placeholder="100.00"
        bind:value={form.weeklyCostLimit}
        disabled={!canManage}
        aria-invalid={errors.weeklyCostLimit ? 'true' : undefined}
        aria-describedby={errors.weeklyCostLimit
          ? 'weekly-budget-error'
          : undefined}
      />{#if errors.weeklyCostLimit}<small
          class="field-error"
          id="weekly-budget-error">{errors.weeklyCostLimit}</small
        >{/if}
    </div>
    <div class="form-field">
      <label for="monthly-budget">Monthly cost budget (optional)</label><input
        id="monthly-budget"
        inputmode="decimal"
        placeholder="100.00"
        bind:value={form.monthlyCostLimit}
        disabled={!canManage}
        aria-invalid={errors.monthlyCostLimit ? 'true' : undefined}
        aria-describedby={errors.monthlyCostLimit
          ? 'monthly-budget-error'
          : undefined}
      />{#if errors.monthlyCostLimit}<small
          class="field-error"
          id="monthly-budget-error">{errors.monthlyCostLimit}</small
        >{/if}
    </div>
  </div>
  {#if editing && services.limitsEnforced}
    {@const daily = budgetWindowState(editing.budget, 'daily')}
    {@const monthly = budgetWindowState(editing.budget, 'monthly')}
    {@const dailyNote = budgetStateNote(daily)}
    {@const monthlyNote = budgetStateNote(monthly)}
    <div class="budget-detail" role="region" aria-label="Current spend budget">
      <div>
        <span>Daily accrued / limit</span>
        <strong class:danger-text={daily === 'exhausted'}
          >{formatBudget(editing.budget.daily.accrued)} / {editing.budget.daily
            .limit === null
            ? 'No limit'
            : formatBudget(currentBudgetLimit(editing.budget.daily))}</strong
        >
        <small
          >Window ends (local time) {formatDate(
            editing.budget.daily.window_ends_at
          )}</small
        >
        {#if dailyNote}<small class:danger-text={daily === 'exhausted'}
            >{dailyNote}</small
          >{/if}
      </div>
      {#if editing.budget.weekly}{@const weekly = budgetWindowState(
          editing.budget,
          'weekly'
        )}{@const weeklyNote = budgetStateNote(weekly)}
        <div>
          <span>Weekly accrued / limit</span>
          <strong class:danger-text={weekly === 'exhausted'}
            >{formatBudget(editing.budget.weekly.accrued)} / {editing.budget
              .weekly.limit === null
              ? 'No limit'
              : formatBudget(currentBudgetLimit(editing.budget.weekly))}</strong
          >
          <small
            >Window ends (local time) {formatDate(
              editing.budget.weekly.window_ends_at
            )}</small
          >
          {#if weeklyNote}<small class:danger-text={weekly === 'exhausted'}
              >{weeklyNote}</small
            >{/if}
        </div>{/if}
      <div>
        <span>Monthly accrued / limit</span>
        <strong class:danger-text={monthly === 'exhausted'}
          >{formatBudget(editing.budget.monthly.accrued)} / {editing.budget
            .monthly.limit === null
            ? 'No limit'
            : formatBudget(currentBudgetLimit(editing.budget.monthly))}</strong
        >
        <small
          >Window ends (local time) {formatDate(
            editing.budget.monthly.window_ends_at
          )}</small
        >
        {#if monthlyNote}<small class:danger-text={monthly === 'exhausted'}
            >{monthlyNote}</small
          >{/if}
      </div>
      <div>
        <span>Unpriced attempts this UTC month</span>
        <strong>{formatInteger(editing.budget.unpriced_attempts)}</strong>
        <small>Unpriced attempts accrue 0 toward cost budgets.</small>
      </div>
    </div>
  {/if}
</section>

<style>
  h2 {
    margin: 0 0 0.75rem;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  section {
    padding-top: 1.5rem;
    border-top: 1px solid var(--border-hairline);
  }
  .budget-inputs {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .budget-detail {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.75rem;
    margin-top: 1rem;
  }
  .budget-detail div {
    display: grid;
    gap: 0.35rem;
    padding: 1rem;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
  }
  .budget-detail span {
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  .budget-detail strong {
    font-size: 1.25rem;
    font-weight: 400;
    font-variant-numeric: tabular-nums;
    letter-spacing: -0.02em;
    line-height: 1.2;
  }
  .budget-detail small {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .budget-detail .danger-text {
    color: var(--danger);
  }
  @media (max-width: 48rem) {
    .budget-inputs,
    .budget-detail {
      grid-template-columns: 1fr;
    }
  }
</style>
