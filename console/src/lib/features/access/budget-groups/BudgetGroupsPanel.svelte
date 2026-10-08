<script lang="ts">
  import { currentBudgetLimit } from '../api-keys/budgetPresentation';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { useServiceCapabilities } from '$lib/features/access/session/serviceCapabilities.svelte';
  import ProjectScopeField from '$lib/features/access/projects/ProjectScopeField.svelte';
  import {
    createBudgetGroup,
    listBudgetGroups,
    updateBudgetGroup,
    type BudgetGroup
  } from '$lib/features/access/budget-groups/api';
  import { budgetGroupKeys } from '$lib/features/access/budget-groups/budgetGroupKeys';
  import { formatBudget, formatDate, formatInteger } from '$lib/format';
  import { limitsError } from '../budgets/limitForm';

  const services = useServiceCapabilities();
  const queryClient = useQueryClient();
  const access = useRole();
  const canManage = $derived(access.can('api_keys.manage'));

  const groups = createQuery(() => ({
    queryKey: budgetGroupKeys.list(),
    queryFn: ({ signal }) => listBudgetGroups(signal)
  }));
  const items = $derived(groups.data ?? []);

  let createName = $state('');
  let createProjectId = $state('');
  let createDaily = $state('');
  let createMonthly = $state('');
  let createWeekly = $state('');
  let createTemplate = $state('');
  let createRates = $state({
    requests_per_minute: '',
    tokens_per_minute: '',
    max_concurrency: ''
  });
  const rateFields = [
    'requests_per_minute',
    'tokens_per_minute',
    'max_concurrency'
  ] as const;
  let createBusy = $state(false);
  let error = $state('');
  let notice = $state('');

  let editingId = $state('');
  let editName = $state('');
  let editDaily = $state('');
  let editMonthly = $state('');
  let editWeekly = $state('');
  let editTemplate = $state('');
  let editRates = $state({
    requests_per_minute: '',
    tokens_per_minute: '',
    max_concurrency: ''
  });
  let editBusy = $state(false);

  function optionalDecimal(value: string): string | null {
    return value.trim() || null;
  }

  function startEdit(group: BudgetGroup) {
    editingId = group.id;
    editName = group.name;
    editDaily = group.daily_cost_limit ?? '';
    editMonthly = group.monthly_cost_limit ?? '';
    editWeekly = group.weekly_cost_limit ?? '';
    editTemplate = group.limit_template ?? '';
    for (const f of rateFields) editRates[f] = group[f]?.toString() ?? '';
    error = '';
  }

  async function submitCreate(event: SubmitEvent) {
    event.preventDefault();
    if (!canManage || createBusy || !createName.trim()) return;
    if (
      !createTemplate.trim() &&
      !Object.values(createRates).some(Boolean) &&
      !createDaily.trim() &&
      !createMonthly.trim() &&
      !createWeekly.trim()
    ) {
      error = 'Set a limit or reference a project template.';
      return;
    }
    const validation =
      limitsError({
        ...createRates,
        daily_cost_limit: createDaily,
        weekly_cost_limit: createWeekly,
        monthly_cost_limit: createMonthly
      }) ||
      (createTemplate &&
      (!createProjectId ||
        !/^[a-z0-9][a-z0-9._-]{0,99}$/.test(createTemplate.trim()))
        ? 'Choose a project and a valid template name.'
        : '');
    if (validation) {
      error = validation;
      return;
    }
    createBusy = true;
    error = notice = '';
    try {
      await createBudgetGroup({
        name: createName.trim(),
        project_id: createProjectId || null,
        daily_cost_limit: optionalDecimal(createDaily),
        weekly_cost_limit: optionalDecimal(createWeekly),
        monthly_cost_limit: optionalDecimal(createMonthly),
        limit_template: createTemplate.trim() || null,
        requests_per_minute: createRates.requests_per_minute
          ? Number(createRates.requests_per_minute)
          : null,
        tokens_per_minute: createRates.tokens_per_minute
          ? Number(createRates.tokens_per_minute)
          : null,
        max_concurrency: createRates.max_concurrency
          ? Number(createRates.max_concurrency)
          : null
      });
      createName = '';
      createTemplate = '';
      for (const f of rateFields) createRates[f] = '';
      createDaily = '';
      createMonthly = '';
      createWeekly = '';
      notice = 'Budget group created.';
      await queryClient.invalidateQueries({ queryKey: budgetGroupKeys.root });
    } catch (cause) {
      error = errorMessage(cause, 'The budget group could not be created.');
    } finally {
      createBusy = false;
    }
  }

  async function submitEdit(group: BudgetGroup) {
    if (!canManage || editBusy || !editName.trim()) return;
    if (
      !editTemplate.trim() &&
      !Object.values(editRates).some(Boolean) &&
      !editDaily.trim() &&
      !editMonthly.trim() &&
      !editWeekly.trim()
    ) {
      error = 'Set a limit or reference a project template.';
      return;
    }
    const validation =
      limitsError({
        ...editRates,
        daily_cost_limit: editDaily,
        weekly_cost_limit: editWeekly,
        monthly_cost_limit: editMonthly
      }) ||
      (editTemplate &&
      (!group.project_id ||
        !/^[a-z0-9][a-z0-9._-]{0,99}$/.test(editTemplate.trim()))
        ? 'Use a template from this group’s project.'
        : '');
    if (validation) {
      error = validation;
      return;
    }
    editBusy = true;
    error = notice = '';
    try {
      await updateBudgetGroup(group, {
        name: editName.trim(),
        daily_cost_limit: optionalDecimal(editDaily),
        weekly_cost_limit: optionalDecimal(editWeekly),
        monthly_cost_limit: optionalDecimal(editMonthly),
        limit_template: editTemplate.trim() || null,
        requests_per_minute: editRates.requests_per_minute
          ? Number(editRates.requests_per_minute)
          : null,
        tokens_per_minute: editRates.tokens_per_minute
          ? Number(editRates.tokens_per_minute)
          : null,
        max_concurrency: editRates.max_concurrency
          ? Number(editRates.max_concurrency)
          : null
      });
      editingId = '';
      notice = 'Budget group updated.';
      await queryClient.invalidateQueries({ queryKey: budgetGroupKeys.root });
    } catch (cause) {
      error = errorMessage(cause, 'The budget group could not be updated.');
    } finally {
      editBusy = false;
    }
  }
</script>

<section class="card groups-panel" aria-labelledby="budget-groups-heading">
  <p class="eyebrow">Shared budgets</p>
  <h2 id="budget-groups-heading">Budget groups</h2>
  <p class="section-help">
    {#if services.limitsEnforced}Keys assigned to a group share its cost budget,
      which counts the spend so far and the estimated cost of members' requests
      still running, while keeping their own limits and history. Windows reset
      at midnight UTC; unpriced attempts accrue 0.{:else}Groups save shared
      budget policy for future enforcement. Spending is not restricted yet.{/if}
  </p>

  {#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
  {#if notice}<p class="section-help" role="status">{notice}</p>{/if}

  {#if canManage}
    <form class="create-form" onsubmit={submitCreate}>
      <div class="form-grid">
        <div class="form-field">
          <label for="group-name">Group name</label><input
            id="group-name"
            bind:value={createName}
            required
          />
        </div>
        <ProjectScopeField id="group-project" bind:value={createProjectId} />
        <div class="form-field">
          <label for="group-template">Limit template</label><input
            id="group-template"
            bind:value={createTemplate}
            maxlength="100"
          />
        </div>
        {#each rateFields as field (field)}<div class="form-field">
            <label for={`group-${field}`}>{field.replaceAll('_', ' ')}</label
            ><input
              id={`group-${field}`}
              bind:value={createRates[field]}
              inputmode="numeric"
            />
          </div>{/each}
        <div class="form-field">
          <label for="group-daily">Daily cost limit</label><input
            id="group-daily"
            inputmode="decimal"
            placeholder="10.00"
            bind:value={createDaily}
          />
        </div>
        <div class="form-field">
          <label for="group-weekly">Weekly cost limit</label><input
            id="group-weekly"
            inputmode="decimal"
            placeholder="100.00"
            bind:value={createWeekly}
          />
        </div>
        <div class="form-field">
          <label for="group-monthly">Monthly cost limit</label><input
            id="group-monthly"
            inputmode="decimal"
            placeholder="100.00"
            bind:value={createMonthly}
          />
        </div>
      </div>
      <div class="form-actions">
        <button
          class="button button-primary"
          type="submit"
          disabled={createBusy || !createName.trim()}
          >{createBusy ? 'Creating…' : 'Create group'}</button
        >
      </div>
    </form>
  {/if}

  {#if groups.isPending}<span class="inline-status" role="status"
      >Loading budget groups…</span
    >
  {:else if groups.isError}<span class="inline-problem" role="alert"
      >Budget groups are unavailable.
      <button class="text-button" type="button" onclick={() => groups.refetch()}
        >Retry</button
      ></span
    >
  {:else if !items.length}<p class="section-help">
      No budget groups yet. Assign keys to a group from the key policy form.
    </p>
  {:else}
    <div class="table-scroll">
      <table class="data-table">
        <thead
          ><tr
            ><th scope="col">Name</th><th scope="col">Project</th><th
              scope="col">Daily</th
            ><th scope="col">Weekly</th><th scope="col">Monthly</th><th
              scope="col">Unpriced</th
            >{#if canManage}<th scope="col"
                ><span class="sr-only">Actions</span></th
              >{/if}</tr
          ></thead
        >
        <tbody>
          {#each items as group (group.id)}
            {#if editingId === group.id}
              <tr
                ><td
                  ><input
                    aria-label="Group name"
                    bind:value={editName}
                    required
                  /></td
                ><td>{group.project_name ?? 'Installation-wide'}</td><td
                  ><input
                    aria-label="Daily cost limit"
                    inputmode="decimal"
                    bind:value={editDaily}
                  /></td
                ><td
                  ><input
                    aria-label="Weekly cost limit"
                    inputmode="decimal"
                    bind:value={editWeekly}
                  /></td
                ><td
                  ><input
                    aria-label="Monthly cost limit"
                    inputmode="decimal"
                    bind:value={editMonthly}
                  /></td
                ><td>{formatInteger(group.budget.unpriced_attempts)}</td><td
                  ><button
                    class="text-button"
                    type="button"
                    disabled={editBusy}
                    onclick={() => submitEdit(group)}
                    >{editBusy ? 'Saving…' : 'Save'}</button
                  >
                  <button
                    class="text-button"
                    type="button"
                    disabled={editBusy}
                    onclick={() => (editingId = '')}>Cancel</button
                  ></td
                ></tr
              >
            {:else}
              <tr
                ><td
                  ><strong>{group.name}</strong>{#if group.limit_template}<br
                    /><small>Template: {group.limit_template}</small
                    >{/if}{#each rateFields as field (field)}{#if group.effective_limits?.[field] != null}<br
                      /><small
                        >{field.replaceAll('_', ' ')}: {group
                          .effective_limits?.[field]}</small
                      >{/if}{/each}</td
                ><td>{group.project_name ?? 'Installation-wide'}</td><td
                  >{formatBudget(group.budget.daily.accrued)} / {group.budget
                    .daily.limit === null
                    ? 'No limit'
                    : formatBudget(currentBudgetLimit(group.budget.daily))}<br
                  /><small
                    >resets {formatDate(group.budget.daily.reset_at)}</small
                  ></td
                ><td
                  >{#if group.budget.weekly}{formatBudget(
                      group.budget.weekly.accrued
                    )} / {currentBudgetLimit(group.budget.weekly) === null
                      ? 'No limit'
                      : formatBudget(
                          currentBudgetLimit(group.budget.weekly)
                        )}<br /><small
                      >resets {formatDate(group.budget.weekly.reset_at)}</small
                    >{:else}—{/if}</td
                ><td
                  >{formatBudget(group.budget.monthly.accrued)} / {group.budget
                    .monthly.limit === null
                    ? 'No limit'
                    : formatBudget(currentBudgetLimit(group.budget.monthly))}<br
                  /><small
                    >resets {formatDate(group.budget.monthly.reset_at)}</small
                  ></td
                ><td>{formatInteger(group.budget.unpriced_attempts)}</td
                >{#if canManage}<td
                    ><button
                      class="text-button"
                      type="button"
                      onclick={() => startEdit(group)}>Edit</button
                    ></td
                  >{/if}</tr
              >
            {/if}
          {/each}
        </tbody>
      </table>
    </div>
    {#if items.some((group) => !group.budget.enforcement_active)}
      <p class="section-help">
        Limits are stored policy only; accrued amounts are not live accounting
        on this installation.
      </p>
    {/if}
  {/if}
</section>

<style>
  .groups-panel {
    display: grid;
    gap: 1rem;
    margin-top: 1.5rem;
    padding: 1.5rem;
  }
  .groups-panel h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .section-help {
    margin: 0;
    color: var(--foreground-muted);
    font-size: 0.8rem;
  }
  .create-form {
    display: grid;
    gap: 1rem;
    padding: 1rem;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
  }
  .form-actions {
    display: flex;
    justify-content: flex-end;
  }
  .table-scroll {
    overflow-x: auto;
  }
  td small {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
</style>
