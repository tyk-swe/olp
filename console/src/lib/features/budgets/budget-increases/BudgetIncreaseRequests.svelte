<script lang="ts">
  import EntityPicker from '#lib/components/EntityPicker.svelte';
  import { projectOptions } from '#lib/domain/projects/projectOptions.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { formatDate } from '#lib/format.js';
  import FormField from '#lib/components/FormField.svelte';
  import { useMutation } from '#lib/editing/action.svelte.js';
  import { focusFormError } from '#lib/editing/focusError.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import {
    approveIncreaseRequest,
    createIncreaseRequest,
    declineIncreaseRequest,
    listIncreaseRequests,
    type BudgetIncreaseRequest
  } from './api';
  import {
    increaseRequestDraft,
    increaseRequestServerFields,
    validateIncreaseRequest,
    type IncreaseRequestField
  } from './increaseRequestDraft';
  import { budgetIncreaseKeys } from './budgetIncreaseKeys';
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import StatusBadge from '#lib/components/StatusBadge.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { confirmAction } from '#lib/components/confirm.js';
  const access = useRole();
  const client = useQueryClient();
  let before = $state<string | undefined>();
  const query = createQuery(() => ({
    queryKey: budgetIncreaseKeys.requestPage(before),
    queryFn: ({ signal }) => listIncreaseRequests(before, signal)
  }));
  const canRequest = $derived(
    access.allows('POST /api/v1/budget-increase-requests')
  );
  const canReview = $derived(
    access.allows('POST /api/v1/budget-increase-requests/{request_id}/approve')
  );
  let creating = $state(false);
  const draft = $state(increaseRequestDraft());
  const refresh = async () => {
    await client.invalidateQueries({
      queryKey: budgetIncreaseKeys.requestsRoot
    });
    await client.invalidateQueries({ queryKey: budgetIncreaseKeys.root });
  };
  const create = useMutation<IncreaseRequestField>(refresh);
  const decide = useMutation(refresh);
  async function submit(
    event: SubmitEvent & { currentTarget: HTMLFormElement }
  ) {
    event.preventDefault();
    const form = event.currentTarget;
    if (!canRequest) return;
    const checked = validateIncreaseRequest($state.snapshot(draft));
    if (!create.check(checked)) return focusFormError(form);
    const accepted = await create.submit(
      checked.value,
      (input, requestId) => createIncreaseRequest(input, requestId),
      async () => {
        creating = false;
        draft.amount = '';
        draft.reason = '';
        notify('Budget increase requested');
        await refresh();
      },
      { serverFields: increaseRequestServerFields }
    );
    if (!accepted) await focusFormError(form);
  }
  async function approve(item: BudgetIncreaseRequest) {
    if (
      !(await confirmAction({
        title: 'Approve this increase?',
        body: `The project budget cap rises by $${item.amount} until the ${item.window_kind} window ends.`,
        confirmLabel: 'Approve increase'
      }))
    )
      return;
    await decide.submit(
      { item, decision: 'approve' as const },
      (input, requestId) => approveIncreaseRequest(input.item, requestId),
      async () => {
        notify('Budget increase approved');
        await refresh();
      }
    );
  }
  async function decline(item: BudgetIncreaseRequest) {
    if (
      !(await confirmAction({
        title: 'Decline this request?',
        body: 'The budget cap stays as it is, and the requester can ask again.',
        confirmLabel: 'Decline request',
        danger: true
      }))
    )
      return;
    await decide.submit(
      { item, decision: 'decline' as const },
      (input, requestId) => declineIncreaseRequest(input.item, requestId),
      async () => {
        notify('Budget increase request declined');
        await refresh();
      }
    );
  }
  const statusTone = (status: BudgetIncreaseRequest['status']) =>
    status === 'approved'
      ? 'success'
      : status === 'pending'
        ? 'warning'
        : 'neutral';
</script>

<section>
  <div class="toolbar">
    <div>
      <h2>Increase requests</h2>
      <p class="lede">
        Members ask for one exact amount against a project budget in its current
        window. A reviewer other than the requester approves or declines, and
        asking never changes the serving cap.
      </p>
    </div>
    {#if canRequest}<div class="page-actions">
        <button
          class="button button-primary"
          type="button"
          onclick={() => (creating = true)}
        >
          Request increase
        </button>
      </div>{/if}
  </div>
  {#if decide.error && !creating}<Callout tone="danger">
      {decide.error}
    </Callout>{/if}
  {#if canRequest && creating}
    <Dialog
      title="Request a budget increase"
      placement="side"
      onClose={() => (creating = false)}
    >
      {#if create.error}<Callout tone="danger">{create.error}</Callout>{/if}
      <form novalidate onsubmit={submit}>
        <fieldset disabled={create.busy}>
          <div class="fields">
            <FormField label="Project" error={create.fieldErrors.project_id}>
              {#snippet control(attributes)}
                <EntityPicker
                  {...attributes}
                  source={projectOptions}
                  bind:value={draft.project_id}
                  emptyLabel="Choose one"
                  required
                />
              {/snippet}
            </FormField>
            <FormField label="Budget window" error={create.fieldErrors.window}>
              {#snippet control(attributes)}
                <select {...attributes} bind:value={draft.window}>
                  <option value="day">Day</option><option value="week">
                    Week
                  </option><option value="month">Month</option>
                </select>
              {/snippet}
            </FormField>
            <FormField label="Additional USD" error={create.fieldErrors.amount}>
              {#snippet control(attributes)}
                <input
                  {...attributes}
                  bind:value={draft.amount}
                  inputmode="decimal"
                  required
                />
              {/snippet}
            </FormField>
            <FormField label="Reason" error={create.fieldErrors.reason}>
              {#snippet control(attributes)}
                <input
                  {...attributes}
                  bind:value={draft.reason}
                  required
                  maxlength="256"
                /><small>
                  The request binds to the current window and cap, so a changed
                  cap or a new window needs a new request.
                </small>
              {/snippet}
            </FormField>
          </div>
        </fieldset>
        <button
          class="button button-primary"
          disabled={create.busy}
          type="submit"
        >
          Request increase
        </button>
      </form>
    </Dialog>
  {/if}
  {#if query.isPending}<p class="loading-state" role="status">
      Loading requests…
    </p>{:else if query.isError}<Callout
      tone="danger"
      title="Requests could not be loaded"
    >
      {errorMessage(query.error)}{#snippet actions()}<button
          type="button"
          class="button button-secondary"
          onclick={() => query.refetch()}
        >
          Retry
        </button>{/snippet}
    </Callout>{:else if !query.data?.items.length && !before}<EmptyState
      title="No budget increase requests"
    >
      Requests from project members show up here for review.
    </EmptyState>{:else}
    <ScrollTable label="Budget increase requests">
      <table class="data-table">
        <thead>
          <tr>
            <th scope="col">Project</th><th scope="col">Amount</th><th
              scope="col"
            >
              Reason
            </th><th scope="col">Status</th><th scope="col">Requested</th><th
              scope="col"
            >
              <span class="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {#each query.data?.items ?? [] as item (item.id)}
            <tr>
              <td><code class="mono">{item.project_id}</code></td>
              <td class="mono">
                +${item.amount}<span class="muted">
                  {item.window_kind} window on a ${item.base_cap} cap
                </span>
              </td>
              <td>{item.reason}</td>
              <td>
                <StatusBadge
                  label={item.status}
                  tone={statusTone(item.status)}
                />
              </td>
              <td>
                {formatDate(item.created_at)}<span class="muted">
                  {item.requested_by}
                </span>
              </td>
              <td>
                {#if canReview && item.status === 'pending'}
                  {#if item.requested_by !== access.user?.id}<button
                      type="button"
                      class="text-button"
                      disabled={decide.busy}
                      onclick={() => approve(item)}
                    >
                      Approve
                    </button>{:else}<span class="muted">
                      Another reviewer must approve this
                    </span>{/if}
                  <button
                    type="button"
                    class="text-button danger"
                    disabled={decide.busy}
                    onclick={() => decline(item)}
                  >
                    Decline
                  </button>
                {/if}
              </td>
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
          }}
        >
          Newest
        </button>{/if}{#if query.data?.next_cursor}<button
          type="button"
          class="button button-secondary"
          onclick={() => {
            before = query.data?.next_cursor ?? undefined;
          }}
        >
          Older
        </button>{/if}
    </div>
  {/if}
</section>

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
