<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import NotificationDestinationTest from './NotificationDestinationTest.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { notificationKeys } from './notificationKeys';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction, useMutation } from '#lib/editing/action.svelte.js';
  import { focusFormError } from '#lib/editing/focusError.js';
  import QueryView from '#lib/components/QueryView.svelte';
  import NotificationDestinationDialog from './NotificationDestinationDialog.svelte';
  import {
    createNotificationDestination,
    deleteNotificationDestination,
    listNotificationDestinations,
    updateNotificationDestination,
    type NotificationDestination
  } from './api';
  import {
    canChangeNotification,
    isChat,
    secretRequired
  } from './presentation';
  import {
    createDestinationInput,
    destinationRowServerFields,
    editDestinationDraft,
    newDestinationDraft,
    validateDestinationChange,
    type DestinationDraft,
    type DestinationField
  } from './destinationDraft';
  const queryClient = useQueryClient();
  let {
    canManage,
    installationWide,
    canDelete
  }: { canManage: boolean; installationWide: boolean; canDelete: boolean } =
    $props();
  const canChange = (destination: NotificationDestination) =>
    canChangeNotification(destination.project_id, {
      project: canManage,
      installation: installationWide
    });
  let creating = $state(false);
  const create = useMutation();
  const edit = useAction<DestinationField>();
  const toggle = useAction();
  const problem = $derived(create.error || edit.error || toggle.error);
  const destinations = createQuery(() => ({
    queryKey: notificationKeys.destinations(),
    queryFn: ({ signal }) => listNotificationDestinations(signal)
  }));
  function submitDestination(draft: DestinationDraft) {
    if (!canManage) return Promise.resolve(false);
    return create.submit(
      createDestinationInput(draft),
      (input, requestId) => createNotificationDestination(input, requestId),
      async () => {
        creating = false;
        notify('Destination created');
        await queryClient.invalidateQueries(
          {
            queryKey: notificationKeys.root
          },
          { throwOnError: true }
        );
      },
      {
        failure: (cause, stage) =>
          stage === 'accept'
            ? `Destination created, but refreshing failed. ${errorMessage(cause)}`
            : errorMessage(cause, 'The destination could not be created.')
      }
    );
  }

  let editDestId = $state('');
  let editDraft = $state<DestinationDraft>(newDestinationDraft('webhook'));
  let editDestination = $state.raw<NotificationDestination | null>(null);
  let editDestSecret = $state('');
  let editDestClear = $state(false);
  let table = $state<HTMLTableElement>();
  const id = $props.id();
  function reported(field: DestinationField) {
    return edit.fieldErrors[field]
      ? {
          'aria-invalid': 'true' as const,
          'aria-describedby': `${id}-${field}`
        }
      : {};
  }

  function startDestEdit(destination: NotificationDestination) {
    if (edit.busy || !canChange(destination)) return;
    editDestination = destination;
    editDestId = destination.id;
    editDraft = editDestinationDraft(destination);
    editDestSecret = '';
    editDestClear = false;
    edit.reset();
  }

  async function saveDestination(destination: NotificationDestination) {
    if (
      !canChange(destination) ||
      edit.busy ||
      !editDraft.name.trim() ||
      !editDraft.url.trim()
    )
      return;
    const current = editDestination;
    if (!current || current.id !== destination.id) return;
    const checked = validateDestinationChange(
      editDraft,
      editDestSecret,
      editDestClear
    );
    if (!edit.check(checked)) return focusTable();
    const saved = await edit.run(
      () => updateNotificationDestination(current, checked.value),
      async () => {
        editDestId = '';
        editDestination = null;
        editDestSecret = '';
        notify('Destination updated.');
        await queryClient.invalidateQueries(
          {
            queryKey: notificationKeys.root
          },
          { throwOnError: true }
        );
      },
      {
        failure: (cause, stage) =>
          stage === 'accept'
            ? `Destination updated, but refreshing failed. ${errorMessage(cause)}`
            : errorMessage(cause, 'The destination could not be updated.'),
        serverFields: destinationRowServerFields
      }
    );
    if (!saved) await focusTable();
  }

  async function focusTable() {
    if (table) await focusFormError(table);
  }

  async function toggleDestination(destination: NotificationDestination) {
    if (!canChange(destination)) return;
    await toggle.run(
      () =>
        updateNotificationDestination(destination, {
          enabled: !destination.enabled
        }),
      () =>
        queryClient.invalidateQueries({
          queryKey: notificationKeys.root
        }),
      {
        failure: (cause) =>
          errorMessage(cause, 'The destination could not be updated.')
      }
    );
  }

  usePrincipalLifetime(() => {
    editDestId = editDestSecret = '';
    editDraft = newDestinationDraft('webhook');
    editDestination = null;
  });
</script>

{#snippet problemAt(field: DestinationField)}
  {#if edit.fieldErrors[field]}<p class="field-error" id="{id}-{field}">
      {edit.fieldErrors[field]}
    </p>{/if}
{/snippet}

{#if problem && !creating}<Callout tone="danger">{problem}</Callout>{/if}

{#if canManage}<div class="toolbar">
    <p class="section-help">
      Where alerts are sent, such as a webhook or an email address.
    </p>
    <div class="page-actions">
      <button
        class="button button-primary"
        type="button"
        onclick={() => {
          create.dismiss();
          creating = true;
        }}
      >
        New destination
      </button>
    </div>
  </div>{/if}
{#if creating}
  <NotificationDestinationDialog
    busy={create.busy}
    error={problem}
    {canManage}
    {installationWide}
    onClose={() => (creating = false)}
    onSubmit={submitDestination}
  />
{/if}

<QueryView
  query={destinations}
  loading="Loading destinations…"
  failure="Destinations are unavailable."
  emptyWhen={!(destinations.data ?? []).length}
>
  {#snippet empty()}
    <EmptyState title="No destinations yet">
      Add a webhook or email destination, then point rules at it.
    </EmptyState>
  {/snippet}
  <ScrollTable label="Notification destinations">
    <table class="data-table" bind:this={table}>
      <thead>
        <tr>
          <th scope="col">Name</th><th scope="col">Type</th><th scope="col">
            URL
          </th><th scope="col">Project</th><th scope="col">Secret</th><th
            scope="col"
          >
            Enabled
          </th>{#if canManage || installationWide}<th scope="col">
              <span class="sr-only">Actions</span>
            </th>{/if}
        </tr>
      </thead>
      <tbody>
        {#each destinations.data ?? [] as destination (destination.id)}
          {#if editDestId === destination.id}
            <tr>
              <td>
                <input
                  {...reported('name')}
                  aria-label="Destination name"
                  bind:value={editDraft.name}
                  disabled={edit.busy}
                  required
                />{@render problemAt('name')}
              </td><td>{destination.type ?? 'webhook'}</td><td>
                <input
                  {...reported('url')}
                  aria-label="Destination URL"
                  type="url"
                  bind:value={editDraft.url}
                  disabled={edit.busy}
                  required
                />{@render problemAt('url')}
              </td><td>
                {destination.project_name ?? 'Installation-wide'}
              </td><td>
                <input
                  {...reported('secret')}
                  aria-label="Replacement secret"
                  bind:value={editDestSecret}
                  disabled={edit.busy}
                  placeholder={isChat(destination.type ?? 'webhook')
                    ? 'New tokenized webhook URL'
                    : destination.type === 'pagerduty'
                      ? 'New routing key'
                      : destination.type === 'email'
                        ? '{"username":"…","password":"…"}'
                        : 'Keep current'}
                />{@render problemAt(
                  'secret'
                )}{#if !secretRequired(destination.type ?? 'webhook') || !destination.enabled}<label
                    class="inline-check"
                  >
                    <input
                      type="checkbox"
                      bind:checked={editDestClear}
                      disabled={edit.busy}
                    />Clear credential
                  </label>{/if}
                {#if editDraft.type === 'email'}<div class="form-grid">
                    <input
                      aria-label="From address"
                      bind:value={editDraft.from}
                      disabled={edit.busy}
                      placeholder="From"
                    /><input
                      aria-label="To addresses"
                      bind:value={editDraft.to}
                      disabled={edit.busy}
                      placeholder="To, comma-separated"
                    /><input
                      aria-label="Subject prefix"
                      bind:value={editDraft.subjectPrefix}
                      disabled={edit.busy}
                      placeholder="Subject prefix"
                    /><input
                      aria-label="CA certificate"
                      bind:value={editDraft.caCertificate}
                      disabled={edit.busy}
                      placeholder="CA certificate (PEM)"
                    />
                  </div>{/if}
              </td><td>{destination.enabled ? 'Yes' : 'No'}</td><td>
                <button
                  class="text-button"
                  type="button"
                  disabled={edit.busy}
                  onclick={() => saveDestination(destination)}
                >
                  {edit.busy ? 'Saving…' : 'Save'}
                </button><button
                  class="text-button"
                  type="button"
                  disabled={edit.busy}
                  onclick={() => {
                    editDestId = '';
                    editDestination = null;
                    editDestSecret = '';
                  }}
                >
                  Cancel
                </button>
              </td>
            </tr>
          {:else}
            <tr>
              <td><strong>{destination.name}</strong></td><td>
                {destination.type ?? 'webhook'}
              </td><td><span class="mono">{destination.url}</span></td><td>
                {destination.project_name ?? 'Installation-wide'}
              </td><td>
                {destination.secret_configured ? 'Configured' : '—'}
              </td><td>
                {destination.enabled ? 'Yes' : 'No'}
              </td>{#if canManage || installationWide}<td>
                  {#if canChange(destination)}<button
                      class="text-button"
                      type="button"
                      disabled={edit.busy}
                      onclick={() => startDestEdit(destination)}
                    >
                      Edit
                    </button><button
                      class="text-button"
                      type="button"
                      disabled={toggle.busy}
                      onclick={() => toggleDestination(destination)}
                    >
                      {destination.enabled ? 'Disable' : 'Enable'}
                    </button>
                    {#if destination.enabled && (destination.type == null || destination.type === 'webhook' || destination.type === 'email')}
                      <NotificationDestinationTest {destination} />
                    {/if}
                    {#if canDelete}<ConfirmAction
                        danger
                        label="Delete destination"
                        description={`Delete ${destination.name}. Referenced resources cannot be deleted`}
                        identity={`${destination.id}:${destination.etag}`}
                        run={(requestId) =>
                          deleteNotificationDestination(destination, requestId)}
                        done={async () => {
                          await queryClient.invalidateQueries({
                            queryKey: notificationKeys.root
                          });
                        }}
                      />{/if}{/if}
                </td>{/if}
            </tr>
          {/if}
        {/each}
      </tbody>
    </table>
  </ScrollTable>
</QueryView>
