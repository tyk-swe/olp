<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import { validRouteSlug } from '#lib/routeIdentity.js';
  import { currentBudgetLimit } from '../budgetLimit.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { useServiceCapabilities } from '#lib/session/serviceCapabilities.svelte.js';
  import {
    retireBudgetGroup,
    createBudgetGroup,
    listBudgetGroups,
    updateBudgetGroup,
    type BudgetGroup
  } from '#lib/features/budgets/budget-groups/api.js';
  import { budgetGroupKeys } from '#lib/features/budgets/budget-groups/budgetGroupKeys.js';
  import { formatBudget, formatDate, formatInteger } from '#lib/format.js';
  import { limitsError } from '../limitForm';

  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction, useMutation } from '#lib/editing/action.svelte.js';
  import BudgetGroupCreateDialog from './BudgetGroupCreateDialog.svelte';
  import QueryView from '#lib/components/QueryView.svelte';

  const create = useMutation();
  const edit = useAction();

  const services = useServiceCapabilities();
  const queryClient = useQueryClient();
  const access = useRole();
  const canManage = $derived(access.can('api_keys.manage'));

  const groups = createQuery(() => ({
    queryKey: budgetGroupKeys.list(),
    queryFn: ({ signal }) => listBudgetGroups(signal)
  }));
  const items = $derived(groups.data ?? []);

  let creating = $state(false);

  const rateFields = [
    'requests_per_minute',
    'tokens_per_minute',
    'max_concurrency'
  ] as const;

  let editingId = $state('');
  let editingGroup = $state.raw<BudgetGroup | null>(null);
  let editName = $state('');
  let editBasis = $state('cost');
  let editDaily = $state('');
  let editMonthly = $state('');
  let editWeekly = $state('');
  let editTemplate = $state('');
  const editRates = $state({
    requests_per_minute: '',
    tokens_per_minute: '',
    max_concurrency: ''
  });

  function optionalDecimal(value: string): string | null {
    return value.trim() || null;
  }

  usePrincipalLifetime(() => {
    editingId =
      editName =
      editDaily =
      editMonthly =
      editWeekly =
      editTemplate =
        '';
    for (const field of rateFields) editRates[field] = '';
    editingGroup = null;
  });

  function startEdit(group: BudgetGroup) {
    if (edit.busy) return;
    editingGroup = group;
    editingId = group.id;
    editName = group.name;
    editDaily = group.daily_cost_limit ?? '';
    editBasis = group.cost_basis ?? 'cost';
    editMonthly = group.monthly_cost_limit ?? '';
    editWeekly = group.weekly_cost_limit ?? '';
    editTemplate = group.limit_template ?? '';
    for (const f of rateFields) editRates[f] = group[f]?.toString() ?? '';
    edit.error = '';
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
            : errorMessage(cause, 'The budget group could not be created.')
      }
    );
  }

  async function submitEdit(group: BudgetGroup) {
    if (!canManage || edit.busy || !editName.trim()) return;
    const current = editingGroup;
    if (!current || current.id !== group.id) return;
    if (
      !editTemplate.trim() &&
      !Object.values(editRates).some(Boolean) &&
      !editDaily.trim() &&
      !editMonthly.trim() &&
      !editWeekly.trim()
    ) {
      edit.error = 'Set a limit or reference a project template.';
      return;
    }
    const validation =
      limitsError({
        ...editRates,
        cost_basis: editBasis as 'cost' | 'charge',
        daily_cost_limit: editDaily,
        weekly_cost_limit: editWeekly,
        monthly_cost_limit: editMonthly
      }) ||
      (editTemplate &&
      (!group.project_id || !validRouteSlug(editTemplate.trim()))
        ? 'Use a template from this group’s project.'
        : '');
    if (validation) {
      edit.error = validation;
      return;
    }
    await edit.run(
      () =>
        updateBudgetGroup(current, {
          name: editName.trim(),
          cost_basis: editBasis as 'cost' | 'charge',
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
        }),
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
            : errorMessage(cause, 'The budget group could not be updated.')
      }
    );
  }
</script>

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
            create.error = '';
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
      busy={create.busy}
      bind:error={create.error}
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
      <table class="data-table">
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
                <td>
                  <input
                    aria-label="Group name"
                    bind:value={editName}
                    disabled={edit.busy}
                    required
                  />
                </td><td>{group.project_name ?? 'Installation-wide'}</td><td>
                  <input
                    aria-label="Daily cost limit"
                    inputmode="decimal"
                    bind:value={editDaily}
                    disabled={edit.busy}
                  />
                </td><td>
                  <input
                    aria-label="Weekly cost limit"
                    inputmode="decimal"
                    bind:value={editWeekly}
                    disabled={edit.busy}
                  />
                </td><td>
                  <input
                    aria-label="Monthly cost limit"
                    inputmode="decimal"
                    bind:value={editMonthly}
                    disabled={edit.busy}
                  />
                </td><td>
                  <select
                    aria-label="Budget basis"
                    bind:value={editBasis}
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
