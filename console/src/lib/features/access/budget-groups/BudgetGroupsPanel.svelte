<script lang="ts">
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
  let createBusy = $state(false);
  let error = $state('');
  let notice = $state('');

  let editingId = $state('');
  let editName = $state('');
  let editDaily = $state('');
  let editMonthly = $state('');
  let editBusy = $state(false);

  function optionalDecimal(value: string): string | null {
    return value.trim() || null;
  }

  function startEdit(group: BudgetGroup) {
    editingId = group.id;
    editName = group.name;
    editDaily = group.daily_cost_limit ?? '';
    editMonthly = group.monthly_cost_limit ?? '';
    error = '';
  }

  async function submitCreate(event: SubmitEvent) {
    event.preventDefault();
    if (!canManage || createBusy || !createName.trim()) return;
    if (!createDaily.trim() && !createMonthly.trim()) {
      error = 'Set at least one cost limit.';
      return;
    }
    createBusy = true;
    error = notice = '';
    try {
      await createBudgetGroup({
        name: createName.trim(),
        project_id: createProjectId || null,
        daily_cost_limit: optionalDecimal(createDaily),
        monthly_cost_limit: optionalDecimal(createMonthly)
      });
      createName = '';
      createDaily = '';
      createMonthly = '';
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
    if (!editDaily.trim() && !editMonthly.trim()) {
      error = 'Set at least one cost limit.';
      return;
    }
    editBusy = true;
    error = notice = '';
    try {
      await updateBudgetGroup(group, {
        name: editName.trim(),
        daily_cost_limit: optionalDecimal(editDaily),
        monthly_cost_limit: optionalDecimal(editMonthly)
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
    {#if services.limitsEnforced}Keys assigned to a group share its accrued-cost
      budget while keeping their own limits and history. Windows reset at
      midnight UTC; unpriced attempts accrue 0.{:else}Groups save shared budget
      policy for future enforcement. Spending is not restricted yet.{/if}
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
          <label for="group-daily">Daily cost limit</label><input
            id="group-daily"
            inputmode="decimal"
            placeholder="10.00"
            bind:value={createDaily}
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

  {#if groups.isPending}<span role="status">Loading budget groups…</span>
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
            ><th scope="col">Monthly</th><th scope="col">Unpriced</th
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
                ><td><strong>{group.name}</strong></td><td
                  >{group.project_name ?? 'Installation-wide'}</td
                ><td
                  >{formatBudget(group.budget.daily.accrued)} / {group.budget
                    .daily.limit === null
                    ? 'No limit'
                    : formatBudget(group.budget.daily.limit)}<br /><small
                    >resets {formatDate(group.budget.daily.reset_at)}</small
                  ></td
                ><td
                  >{formatBudget(group.budget.monthly.accrued)} / {group.budget
                    .monthly.limit === null
                    ? 'No limit'
                    : formatBudget(group.budget.monthly.limit)}<br /><small
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
