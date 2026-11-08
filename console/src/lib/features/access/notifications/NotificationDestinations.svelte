<script lang="ts">
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import ProjectScopeField from '#lib/features/access/projects/ProjectScopeField.svelte';
  import { notificationKeys } from './notificationKeys';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import {
    createNotificationDestination,
    deleteNotificationDestination,
    listNotificationDestinations,
    updateNotificationDestination,
    type NotificationDestination
  } from './api';
  import { destinationTypes, isChat, secretRequired } from './presentation';
  import {
    createDestinationInput,
    editDestinationDraft,
    newDestinationDraft,
    parseDestinationSecret,
    updateDestinationInput,
    type DestinationDraft
  } from './destinationDraft';
  const queryClient = useQueryClient();
  let {
    canManage,
    installationWide,
    canDelete
  }: { canManage: boolean; installationWide: boolean; canDelete: boolean } =
    $props();
  let error = $state('');
  let notice = $state('');
  const destinationIntent = useMutationIntent();
  const destinations = createQuery(() => ({
    queryKey: notificationKeys.destinations(),
    queryFn: ({ signal }) => listNotificationDestinations(signal)
  }));
  let draft = $state<DestinationDraft>(newDestinationDraft('webhook'));
  let destBusy = $state(false);

  function resetDestinationForm() {
    draft = newDestinationDraft(draft.type, {
      name: '',
      url: '',
      projectId: draft.projectId
    });
  }

  async function submitDestination(event: SubmitEvent) {
    event.preventDefault();
    if (!canManage || destBusy || !draft.name.trim() || !draft.url.trim())
      return;
    destBusy = true;
    const isCurrent = captureOwner();
    error = notice = '';
    let committed = false;
    try {
      const submission = destinationIntent.prepare<
        Parameters<typeof createNotificationDestination>[0]
      >(createDestinationInput(draft));
      await createNotificationDestination(
        submission.input,
        submission.requestId
      );
      if (!isCurrent()) return;
      destinationIntent.reset();
      committed = true;
      resetDestinationForm();
      notice = 'Destination created.';
      await queryClient.invalidateQueries(
        {
          queryKey: notificationKeys.root
        },
        { throwOnError: true }
      );
    } catch (cause) {
      if (!isCurrent()) return;
      error = committed
        ? `Destination created, but refreshing failed. ${errorMessage(cause)}`
        : errorMessage(cause, 'The destination could not be created.');
    } finally {
      if (isCurrent()) destBusy = false;
    }
  }

  let editDestId = $state('');
  let editDraft = $state<DestinationDraft>(newDestinationDraft('webhook'));
  let editDestination = $state.raw<NotificationDestination | null>(null);
  let editDestSecret = $state('');
  let editDestClear = $state(false);
  let editDestBusy = $state(false);

  function startDestEdit(destination: NotificationDestination) {
    if (editDestBusy) return;
    editDestination = destination;
    editDestId = destination.id;
    editDraft = editDestinationDraft(destination);
    editDestSecret = '';
    editDestClear = false;
    error = '';
  }

  async function saveDestination(destination: NotificationDestination) {
    if (
      !canManage ||
      editDestBusy ||
      !editDraft.name.trim() ||
      !editDraft.url.trim()
    )
      return;
    const current = editDestination;
    if (!current || current.id !== destination.id) return;
    editDestBusy = true;
    const isCurrent = captureOwner();
    error = notice = '';
    let committed = false;
    try {
      const parsed = parseDestinationSecret(
        editDraft.type,
        editDestSecret,
        editDestClear
      );
      if (parsed.kind === 'invalid') {
        error = parsed.message;
        return;
      }
      await updateNotificationDestination(
        current,
        updateDestinationInput(editDraft, parsed.change)
      );
      committed = true;
      if (!isCurrent()) return;
      editDestId = '';
      editDestination = null;
      editDestSecret = '';
      notice = 'Destination updated.';
      await queryClient.invalidateQueries(
        {
          queryKey: notificationKeys.root
        },
        { throwOnError: true }
      );
    } catch (cause) {
      if (!isCurrent()) return;
      error = committed
        ? `Destination updated, but refreshing failed. ${errorMessage(cause)}`
        : errorMessage(cause, 'The destination could not be updated.');
    } finally {
      if (isCurrent()) editDestBusy = false;
    }
  }

  async function toggleDestination(destination: NotificationDestination) {
    if (!canManage) return;
    const isCurrent = captureOwner();
    error = notice = '';
    try {
      await updateNotificationDestination(destination, {
        enabled: !destination.enabled
      });
      if (!isCurrent()) return;
      await queryClient.invalidateQueries({
        queryKey: notificationKeys.root
      });
    } catch (cause) {
      if (!isCurrent()) return;
      error = errorMessage(cause, 'The destination could not be updated.');
    }
  }

  const captureOwner = usePrincipalLifetime(() => {
    resetDestinationForm();
    editDestId = editDestSecret = '';
    editDraft = newDestinationDraft('webhook');
    editDestination = null;
    destBusy = editDestBusy = false;
    error = notice = '';
  });
</script>

{#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
{#if notice}<p class="section-help" role="status">{notice}</p>{/if}
<h3>Destinations</h3>
{#if canManage}
  <form class="create-form" onsubmit={submitDestination}>
    <fieldset class="submission-fields" disabled={destBusy}>
      <div class="form-grid">
        <div class="form-field">
          <label for="dest-name">Name</label><input
            id="dest-name"
            bind:value={draft.name}
            required
          />
        </div>
        <div class="form-field">
          <label for="dest-type">Type</label><select
            id="dest-type"
            value={draft.type}
            onchange={(event) => {
              const selected = destinationTypes.find(
                (option) => option.value === event.currentTarget.value
              );
              if (selected)
                draft = newDestinationDraft(selected.value, {
                  name: draft.name,
                  url: draft.url,
                  projectId: draft.projectId
                });
            }}
            >{#each destinationTypes as option (option.value)}<option
                value={option.value}>{option.label}</option
              >{/each}</select
          >
        </div>
        <div class="form-field">
          <label for="dest-url">
            {#if draft.type === 'slack' || draft.type === 'msteams' || draft.type === 'discord'}Channel
              origin URL{:else if draft.type === 'pagerduty'}Events API endpoint{:else if draft.type === 'email'}SMTP
              URL{:else}Webhook URL{/if}</label
          ><input
            id="dest-url"
            type="url"
            bind:value={draft.url}
            required
            placeholder={isChat(draft.type)
              ? 'https://hooks.slack.com'
              : draft.type === 'pagerduty'
                ? 'https://events.pagerduty.com/v2/enqueue'
                : draft.type === 'email'
                  ? 'smtps://smtp.example.com:465'
                  : 'https://hooks.example.com/olp'}
          />
        </div>
        <ProjectScopeField
          id="dest-project"
          bind:value={draft.projectId}
          unassigned={installationWide}
        />
        {#if draft.type === 'slack' || draft.type === 'msteams' || draft.type === 'discord'}
          <div class="form-field">
            <label for="dest-webhook-url">Channel webhook URL</label><input
              id="dest-webhook-url"
              bind:value={draft.webhookUrl}
              autocomplete="off"
              required
              placeholder="Full tokenized webhook URL, sealed"
            />
            <p class="section-help">
              Stored sealed; never shown again. Must share the origin URL above.
            </p>
          </div>
        {:else if draft.type === 'pagerduty'}
          <div class="form-field">
            <label for="dest-routing-key">Routing key</label><input
              id="dest-routing-key"
              bind:value={draft.routingKey}
              autocomplete="off"
              required
              placeholder="PagerDuty Events API routing key, sealed"
            />
          </div>
        {:else if draft.type === 'email'}
          <div class="form-field">
            <label for="dest-email-from">From address</label><input
              id="dest-email-from"
              bind:value={draft.from}
              required
              placeholder="alerts@example.com"
            />
          </div>
          <div class="form-field">
            <label for="dest-email-to">To addresses</label><input
              id="dest-email-to"
              bind:value={draft.to}
              required
              placeholder="ops@example.com, dev@example.com"
            />
          </div>
          <div class="form-field">
            <label for="dest-email-prefix">Subject prefix</label><input
              id="dest-email-prefix"
              bind:value={draft.subjectPrefix}
              placeholder="Optional [OLP] prefix"
            />
          </div>
          <div class="form-field">
            <label for="dest-email-ca">CA certificate (PEM)</label><textarea
              id="dest-email-ca"
              bind:value={draft.caCertificate}
              rows="3"
              placeholder="Optional pinned CA bundle"></textarea>
          </div>
          <div class="form-field">
            <label for="dest-email-username">SMTP username</label><input
              id="dest-email-username"
              bind:value={draft.username}
              autocomplete="off"
              placeholder="Optional"
            />
          </div>
          <div class="form-field">
            <label for="dest-email-password">SMTP password</label><input
              id="dest-email-password"
              type="password"
              bind:value={draft.password}
              autocomplete="new-password"
              placeholder="Optional, sealed"
            />
          </div>
        {:else if draft.type === 'webhook'}
          <div class="form-field">
            <label for="dest-secret">Signing secret</label><input
              id="dest-secret"
              bind:value={draft.secret}
              autocomplete="off"
              placeholder="Optional HMAC secret"
            />
          </div>
        {/if}
      </div>
      <div class="form-actions">
        <button
          class="button button-primary"
          type="submit"
          disabled={destBusy || !draft.name.trim() || !draft.url.trim()}
          >{destBusy ? 'Creating…' : 'Create destination'}</button
        >
      </div>
    </fieldset>
  </form>
{/if}

{#if destinations.isPending}<span class="inline-status" role="status"
    >Loading destinations…</span
  >
{:else if destinations.isError}<span class="inline-problem" role="alert"
    >Destinations are unavailable.
    <button
      class="text-button"
      type="button"
      onclick={() => destinations.refetch()}>Retry</button
    ></span
  >
{:else if !(destinations.data ?? []).length}<p class="section-help">
    No destinations yet.
  </p>
{:else}
  <div class="table-scroll">
    <table class="data-table">
      <thead
        ><tr
          ><th scope="col">Name</th><th scope="col">Type</th><th scope="col"
            >URL</th
          ><th scope="col">Project</th><th scope="col">Secret</th><th
            scope="col">Enabled</th
          >{#if canManage}<th scope="col"
              ><span class="sr-only">Actions</span></th
            >{/if}</tr
        ></thead
      >
      <tbody>
        {#each destinations.data ?? [] as destination (destination.id)}
          {#if editDestId === destination.id}
            <tr
              ><td
                ><input
                  aria-label="Destination name"
                  bind:value={editDraft.name}
                  disabled={editDestBusy}
                  required
                /></td
              ><td>{destination.type ?? 'webhook'}</td><td
                ><input
                  aria-label="Destination URL"
                  type="url"
                  bind:value={editDraft.url}
                  disabled={editDestBusy}
                  required
                /></td
              ><td>{destination.project_name ?? 'Installation-wide'}</td><td
                ><input
                  aria-label="Replacement secret"
                  bind:value={editDestSecret}
                  disabled={editDestBusy}
                  placeholder={isChat(destination.type ?? 'webhook')
                    ? 'New tokenized webhook URL'
                    : destination.type === 'pagerduty'
                      ? 'New routing key'
                      : destination.type === 'email'
                        ? '{"username":"…","password":"…"}'
                        : 'Keep current'}
                />{#if !secretRequired(destination.type ?? 'webhook') || !destination.enabled}<label
                    class="inline-check"
                    ><input
                      type="checkbox"
                      bind:checked={editDestClear}
                      disabled={editDestBusy}
                    />Clear credential</label
                  >{/if}
                {#if editDraft.type === 'email'}<div class="form-grid">
                    <input
                      aria-label="From address"
                      bind:value={editDraft.from}
                      disabled={editDestBusy}
                      placeholder="From"
                    /><input
                      aria-label="To addresses"
                      bind:value={editDraft.to}
                      disabled={editDestBusy}
                      placeholder="To, comma-separated"
                    /><input
                      aria-label="Subject prefix"
                      bind:value={editDraft.subjectPrefix}
                      disabled={editDestBusy}
                      placeholder="Subject prefix"
                    /><input
                      aria-label="CA certificate"
                      bind:value={editDraft.caCertificate}
                      disabled={editDestBusy}
                      placeholder="CA certificate (PEM)"
                    />
                  </div>{/if}</td
              ><td>{destination.enabled ? 'Yes' : 'No'}</td><td
                ><button
                  class="text-button"
                  type="button"
                  disabled={editDestBusy}
                  onclick={() => saveDestination(destination)}
                  >{editDestBusy ? 'Saving…' : 'Save'}</button
                ><button
                  class="text-button"
                  type="button"
                  disabled={editDestBusy}
                  onclick={() => {
                    editDestId = '';
                    editDestination = null;
                    editDestSecret = '';
                  }}>Cancel</button
                ></td
              ></tr
            >
          {:else}
            <tr
              ><td><strong>{destination.name}</strong></td><td
                >{destination.type ?? 'webhook'}</td
              ><td><span class="mono">{destination.url}</span></td><td
                >{destination.project_name ?? 'Installation-wide'}</td
              ><td>{destination.secret_configured ? 'Configured' : '—'}</td><td
                >{destination.enabled ? 'Yes' : 'No'}</td
              >{#if canManage}<td
                  ><button
                    class="text-button"
                    type="button"
                    disabled={editDestBusy}
                    onclick={() => startDestEdit(destination)}>Edit</button
                  ><button
                    class="text-button"
                    type="button"
                    onclick={() => toggleDestination(destination)}
                    >{destination.enabled ? 'Disable' : 'Enable'}</button
                  >
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
                    />{/if}</td
                >{/if}</tr
            >
          {/if}
        {/each}
      </tbody>
    </table>
  </div>
{/if}
