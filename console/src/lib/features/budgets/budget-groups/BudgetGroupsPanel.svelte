<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import { currentBudgetLimit } from '../budgetLimit.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { useServiceCapabilities } from '#lib/session/serviceCapabilities.svelte.js';
  import {
    retireBudgetGroup,
    createBudgetGroup,
    updateBudgetGroup
  } from '#lib/features/budgets/budget-groups/api.js';
  import { type BudgetGroup } from '#lib/domain/budget-groups/api.js';
  import { budgetGroupKeys } from '#lib/domain/budget-groups/budgetGroupKeys.js';
  import { formatBudget, formatDate, formatInteger } from '#lib/format.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction, useMutation } from '#lib/editing/action.svelte.js';
  import { focusFormError } from '#lib/editing/focusError.js';
  import BudgetGroupCreateDialog from './BudgetGroupCreateDialog.svelte';
  import {
    budgetGroupDraft,
    budgetGroupRowServerFields,
    budgetGroupServerFields,
    validateBudgetGroupChange,
    type BudgetGroupField
  } from './budgetGroupDraft';
  import QueryView from '#lib/components/QueryView.svelte';
  import { budgetGroupQueries } from '#lib/domain/budget-groups/queries.js';

  const create = useMutation<BudgetGroupField>();
  const edit = useAction<BudgetGroupField>();
  const id = $props.id();

  const services = useServiceCapabilities();
  const queryClient = useQueryClient();
  const access = useRole();
  const canManage = $derived(access.can('api_keys.manage'));

  const groups = createQuery(budgetGroupQueries.all);
  const items = $derived(groups.data ?? []);

  let creating = $state(false);

  const rateFields = [
    'requests_per_minute',
    'tokens_per_minute',
    'max_concurrency'
  ] as const;

  let editingId = $state('');
  let editingGroup = $state.raw<BudgetGroup | null>(null);
  let editDraft = $state(budgetGroupDraft());
  let table = $state<HTMLTableElement>();

  usePrincipalLifetime(() => {
    editingId = '';
    editDraft = budgetGroupDraft();
    editingGroup = null;
  });

  function startEdit(group: BudgetGroup) {
    if (edit.busy) return;
    editingGroup = group;
    editingId = group.id;
    editDraft = budgetGroupDraft(group);
    edit.reset();
  }

  function submitCreate(input: Parameters<typeof createBudgetGroup>[0]) {
    if (!canManage) return Promise.resolve(false);
    return create.submit(
      input,
      (value, requestId) => createBudgetGroup(value, requestId),
      async () => {
        notify('Budget group created');
        await queryClient.invalidateQueries(
          { queryKey: budgetGroupKeys.root },
          { throwOnError: true }
        );
      },
      {
        failure: (cause, stage) =>
          stage === 'accept'
            ? `Budget group created, but refreshing failed. ${errorMessage(cause)}`
            : errorMessage(cause, 'The budget group could not be created.'),
        serverFields: budgetGroupServerFields
      }
    );
  }

  async function submitEdit(group: BudgetGroup) {
    if (!canManage || edit.busy || !editDraft.name.trim()) return;
    const current = editingGroup;
    if (!current || current.id !== group.id) return;
    const checked = validateBudgetGroupChange($state.snapshot(editDraft));
    if (!edit.check(checked)) {
      if (table) await focusFormError(table);
      return;
    }
    const saved = await edit.run(
      () => updateBudgetGroup(current, checked.value),
      async () => {
        editingId = '';
        editingGroup = null;
        notify('Budget group updated.');
        await queryClient.invalidateQueries(
          { queryKey: budgetGroupKeys.root },
          { throwOnError: true }
        );
      },
      {
        failure: (cause, stage) =>
          stage === 'accept'
            ? `Budget group updated, but refreshing failed. ${errorMessage(cause)}`
            : errorMessage(cause, 'The budget group could not be updated.'),
        serverFields: budgetGroupRowServerFields
      }
    );
    if (!saved && table) await focusFormError(table);
  }
</script>

{#snippet rowField(
  field:
    'name' | 'daily_cost_limit' | 'weekly_cost_limit' | 'monthly_cost_limit',
  label: string
)}
  {@const error = edit.fieldErrors[field]}
  {@const errorId = `${id}-${field}-error`}
  <input
    aria-label={label}
    inputmode={field === 'name' ? undefined : 'decimal'}
    required={field === 'name'}
    bind:value={editDraft[field]}
    disabled={edit.busy}
    aria-invalid={error ? 'true' : undefined}
    aria-describedby={error ? errorId : undefined}
  />
  {#if error}<p class="field-error" id={errorId}>{error}</p>{/if}
{/snippet}

<section class="groups-panel" aria-labelledby="budget-groups-heading">
  <div class="toolbar">
    <h2 id="budget-groups-heading" class="sr-only">Budget groups</h2>
    <p class="section-help">
      Keys in a group share one spending limit and keep their own rate limits.
      {#if !services.limitsEnforced}Limits are saved but not enforced yet.{/if}
    </p>
    {#if canManage}<div class="page-actions">
        <button
          class="button button-primary"
          type="button"
          onclick={() => {
            create.dismiss();
            creating = true;
          }}
        >
          New group
        </button>
      </div>{/if}
  </div>

  {#if (create.error || edit.error) && !creating}<Callout tone="danger">
      {create.error || edit.error}
    </Callout>{/if}

  {#if creating}
    <BudgetGroupCreateDialog
      action={create}
      onClose={() => (creating = false)}
      onSubmit={submitCreate}
    />
  {/if}

  <QueryView
    query={groups}
    loading="Loading budget groups…"
    failure="Budget groups are unavailable."
    emptyWhen={!items.length}
  >
    {#snippet empty()}
      <EmptyState title="No budget groups yet">
        Create a group, then add API keys to it from each key's settings.
      </EmptyState>
    {/snippet}
    <ScrollTable label="Budget groups">
      <table class="data-table" bind:this={table}>
        <thead>
          <tr>
            <th scope="col">Name</th><th scope="col">Project</th><th
              scope="col"
            >
              Daily
            </th><th scope="col">Weekly</th><th scope="col">Monthly</th><th
              scope="col"
            >
              Basis
            </th><th scope="col">Unpriced</th>{#if canManage}<th scope="col">
                <span class="sr-only">Actions</span>
              </th>{/if}
          </tr>
        </thead>
        <tbody>
          {#each items as group (group.id)}
            {#if editingId === group.id}
              <tr>
                <td>{@render rowField('name', 'Group name')}</td><td>
                  {group.project_name ?? 'Installation-wide'}
                </td><td>
                  {@render rowField('daily_cost_limit', 'Daily cost limit')}
                </td><td>
                  {@render rowField('weekly_cost_limit', 'Weekly cost limit')}
                </td><td>
                  {@render rowField('monthly_cost_limit', 'Monthly cost limit')}
                </td><td>
                  <select
                    aria-label="Budget basis"
                    bind:value={editDraft.cost_basis}
                    disabled={edit.busy}
                  >
                    <option value="cost">Provider cost</option><option
                      value="charge"
                    >
                      Consumer charge
                    </option>
                  </select>
                </td><td>
                  {formatInteger(group.budget.unpriced_attempts)}
                </td><td>
                  <button
                    class="text-button"
                    type="button"
                    disabled={edit.busy}
                    onclick={() => submitEdit(group)}
                  >
                    {edit.busy ? 'Saving…' : 'Save'}
                  </button>
                  <button
                    class="text-button"
                    type="button"
                    disabled={edit.busy}
                    onclick={() => {
                      editingId = '';
                      editingGroup = null;
                    }}
                  >
                    Cancel
                  </button>
                </td>
              </tr>
            {:else}
              <tr>
                <td>
                  <strong>{group.name}</strong>{#if group.limit_template}<br
                    /><small>
                      Template: {group.limit_template}
                    </small>{/if}{#each rateFields as field (field)}{#if group.effective_limits?.[field] != null}<br
                      /><small>
                        {field.replaceAll('_', ' ')}: {group.effective_limits?.[
                          field
                        ]}
                      </small>{/if}{/each}
                </td><td>{group.project_name ?? 'Installation-wide'}</td><td>
                  {formatBudget(group.budget.daily.accrued)} / {currentBudgetLimit(
                    group.budget.daily
                  ) === null
                    ? 'No limit'
                    : formatBudget(currentBudgetLimit(group.budget.daily))}<br
                  /><small>
                    resets {formatDate(group.budget.daily.reset_at)}
                  </small>
                </td><td>
                  {#if group.budget.weekly}{formatBudget(
                      group.budget.weekly.accrued
                    )} / {currentBudgetLimit(group.budget.weekly) === null
                      ? 'No limit'
                      : formatBudget(
                          currentBudgetLimit(group.budget.weekly)
                        )}<br /><small>
                      resets {formatDate(group.budget.weekly.reset_at)}
                    </small>{:else}—{/if}
                </td><td>
                  {formatBudget(group.budget.monthly.accrued)} / {currentBudgetLimit(
                    group.budget.monthly
                  ) === null
                    ? 'No limit'
                    : formatBudget(currentBudgetLimit(group.budget.monthly))}<br
                  /><small>
                    resets {formatDate(group.budget.monthly.reset_at)}
                  </small>
                </td><td>
                  {group.budget.cost_basis === 'charge' ? 'Charge' : 'Cost'}
                </td><td>
                  {formatInteger(group.budget.unpriced_attempts)}
                </td>{#if canManage}<td>
                    <button
                      class="text-button"
                      type="button"
                      disabled={edit.busy}
                      onclick={() => startEdit(group)}
                    >
                      Edit
                    </button>
                    {#if access.allows('DELETE /api/v1/budget-groups/{budget_group_id}')}<ConfirmAction
                        danger
                        label="Retire budget group"
                        description={`Retire ${group.name}. Existing dependencies and history are checked before retirement`}
                        identity={`${group.id}:${group.etag}`}
                        run={(requestId) => retireBudgetGroup(group, requestId)}
                        done={async () => {
                          if (editingId === group.id) {
                            editingId = '';
                            editingGroup = null;
                          }
                          await queryClient.invalidateQueries({
                            queryKey: budgetGroupKeys.root
                          });
                        }}
                      />{/if}
                  </td>{/if}
              </tr>
            {/if}
          {/each}
        </tbody>
      </table>
    </ScrollTable>
    {#if items.some((group) => !group.budget.enforcement_active)}
      <p class="section-help">
        Limits are stored policy only; accrued amounts are not live accounting
        on this installation.
      </p>
    {/if}
  </QueryView>
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
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  td small {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
</style>
