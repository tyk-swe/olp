<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { useServiceCapabilities } from '$lib/features/access/session/serviceCapabilities.svelte';
  import ProjectScopeField from '$lib/features/access/projects/ProjectScopeField.svelte';
  import { listApiKeys } from '$lib/features/access/api-keys/api';
  import { apiKeyQueries } from '$lib/features/access/api-keys/apiKeyQueries';
  import { listBudgetGroups } from '$lib/features/access/budget-groups/api';
  import { budgetGroupKeys } from '$lib/features/access/budget-groups/budgetGroupKeys';
  import {
    createBudgetAlertRule,
    createNotificationDestination,
    listBudgetAlertRules,
    listNotificationDeliveries,
    listNotificationDestinations,
    updateBudgetAlertRule,
    updateNotificationDestination,
    type BudgetAlertRule,
    type NotificationDestination
  } from '$lib/features/access/notifications/api';
  import { notificationKeys } from '$lib/features/access/notifications/notificationKeys';
  import { formatDate } from '$lib/format';

  const services = useServiceCapabilities();
  const queryClient = useQueryClient();
  const access = useRole();
  const canManage = $derived(access.can('api_keys.manage'));

  const destinations = createQuery(() => ({
    queryKey: notificationKeys.destinations(),
    queryFn: ({ signal }) => listNotificationDestinations(signal)
  }));
  const rules = createQuery(() => ({
    queryKey: notificationKeys.rules(),
    queryFn: ({ signal }) => listBudgetAlertRules(signal)
  }));
  const deliveries = createQuery(() => ({
    queryKey: notificationKeys.deliveries(),
    queryFn: ({ signal }) => listNotificationDeliveries(undefined, signal)
  }));
  const apiKeys = createQuery(() => ({
    queryKey: apiKeyQueries.list(),
    queryFn: ({ signal }) => listApiKeys(signal)
  }));
  const groups = createQuery(() => ({
    queryKey: budgetGroupKeys.list(),
    queryFn: ({ signal }) => listBudgetGroups(signal)
  }));

  let error = $state('');
  let notice = $state('');

  let destName = $state('');
  let destUrl = $state('');
  let destProjectId = $state('');
  let destSecret = $state('');
  let destBusy = $state(false);

  let editDestId = $state('');
  let editDestName = $state('');
  let editDestUrl = $state('');
  let editDestSecret = $state('');
  let editDestBusy = $state(false);

  function startDestEdit(destination: NotificationDestination) {
    editDestId = destination.id;
    editDestName = destination.name;
    editDestUrl = destination.url;
    editDestSecret = '';
    error = '';
  }

  async function submitDestination(event: SubmitEvent) {
    event.preventDefault();
    if (!canManage || destBusy || !destName.trim() || !destUrl.trim()) return;
    destBusy = true;
    error = notice = '';
    try {
      await createNotificationDestination({
        name: destName.trim(),
        url: destUrl.trim(),
        project_id: destProjectId || null,
        secret: destSecret.trim() || null
      });
      destName = destUrl = destSecret = '';
      notice = 'Destination created.';
      await queryClient.invalidateQueries({
        queryKey: notificationKeys.root
      });
    } catch (cause) {
      error = errorMessage(cause, 'The destination could not be created.');
    } finally {
      destBusy = false;
    }
  }

  async function saveDestination(destination: NotificationDestination) {
    if (
      !canManage ||
      editDestBusy ||
      !editDestName.trim() ||
      !editDestUrl.trim()
    )
      return;
    editDestBusy = true;
    error = notice = '';
    try {
      await updateNotificationDestination(destination, {
        name: editDestName.trim(),
        url: editDestUrl.trim(),
        ...(editDestSecret.trim() ? { secret: editDestSecret.trim() } : {})
      });
      editDestId = '';
      notice = 'Destination updated.';
      await queryClient.invalidateQueries({
        queryKey: notificationKeys.root
      });
    } catch (cause) {
      error = errorMessage(cause, 'The destination could not be updated.');
    } finally {
      editDestBusy = false;
    }
  }

  async function toggleDestination(destination: NotificationDestination) {
    if (!canManage) return;
    error = notice = '';
    try {
      await updateNotificationDestination(destination, {
        enabled: !destination.enabled
      });
      await queryClient.invalidateQueries({
        queryKey: notificationKeys.root
      });
    } catch (cause) {
      error = errorMessage(cause, 'The destination could not be updated.');
    }
  }

  let ruleName = $state('');
  let ruleProjectId = $state('');
  let ruleSubjectKind = $state<'api_key' | 'budget_group'>('api_key');
  let ruleSubjectId = $state('');
  let ruleWindow = $state<'day' | 'month'>('month');
  let ruleThreshold = $state('80');
  let ruleDestinationId = $state('');
  let ruleBusy = $state(false);

  const subjectOptions = $derived(
    ruleSubjectKind === 'api_key'
      ? (apiKeys.data ?? []).filter(
          (key) => (key.project_id ?? null) === (ruleProjectId || null)
        )
      : (groups.data ?? []).filter(
          (group) => (group.project_id ?? null) === (ruleProjectId || null)
        )
  );
  const destinationOptions = $derived(
    (destinations.data ?? []).filter(
      (destination) =>
        (destination.project_id ?? null) === (ruleProjectId || null)
    )
  );

  $effect(() => {
    if (
      ruleSubjectId &&
      !subjectOptions.some((subject) => subject.id === ruleSubjectId)
    )
      ruleSubjectId = '';
    if (
      ruleDestinationId &&
      !destinationOptions.some(
        (destination) => destination.id === ruleDestinationId
      )
    )
      ruleDestinationId = '';
  });

  async function submitRule(event: SubmitEvent) {
    event.preventDefault();
    if (
      !canManage ||
      ruleBusy ||
      !ruleName.trim() ||
      !ruleSubjectId ||
      !ruleDestinationId
    )
      return;
    const threshold = Number(ruleThreshold);
    if (!Number.isInteger(threshold) || threshold < 1 || threshold > 100) {
      error = 'Threshold must be a whole percentage from 1 to 100.';
      return;
    }
    ruleBusy = true;
    error = notice = '';
    try {
      await createBudgetAlertRule({
        name: ruleName.trim(),
        project_id: ruleProjectId || null,
        subject_kind: ruleSubjectKind,
        subject_id: ruleSubjectId,
        window_kind: ruleWindow,
        threshold_percent: threshold,
        destination_id: ruleDestinationId
      });
      ruleName = '';
      ruleSubjectId = '';
      ruleDestinationId = '';
      notice = 'Alert rule created.';
      await queryClient.invalidateQueries({
        queryKey: notificationKeys.root
      });
    } catch (cause) {
      error = errorMessage(cause, 'The alert rule could not be created.');
    } finally {
      ruleBusy = false;
    }
  }

  async function toggleRule(rule: BudgetAlertRule) {
    if (!canManage) return;
    error = notice = '';
    try {
      await updateBudgetAlertRule(rule, { enabled: !rule.enabled });
      await queryClient.invalidateQueries({
        queryKey: notificationKeys.root
      });
    } catch (cause) {
      error = errorMessage(cause, 'The alert rule could not be updated.');
    }
  }

  function subjectLabel(rule: BudgetAlertRule) {
    return `${rule.subject_kind === 'api_key' ? 'API key' : 'Budget group'} · ${rule.subject_name ?? rule.subject_id}`;
  }
</script>

<section
  class="card notifications-panel"
  aria-labelledby="notifications-heading"
>
  <p class="eyebrow">Budget alerts</p>
  <h2 id="notifications-heading">Notification destinations and rules</h2>
  <p class="section-help">
    {#if services.notificationsActive}Crossed budget thresholds post a
      metadata-only webhook to the destination. Rules fire at most once per rule
      and window; failed deliveries retry with backoff.{:else}Alert rules and
      destinations are stored, but this installation is not running the delivery
      worker, so no notifications will be sent yet.{/if}
  </p>

  {#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
  {#if notice}<p class="section-help" role="status">{notice}</p>{/if}

  <h3>Destinations</h3>
  {#if canManage}
    <form class="create-form" onsubmit={submitDestination}>
      <div class="form-grid">
        <div class="form-field">
          <label for="dest-name">Name</label><input
            id="dest-name"
            bind:value={destName}
            required
          />
        </div>
        <div class="form-field">
          <label for="dest-url">Webhook URL</label><input
            id="dest-url"
            type="url"
            bind:value={destUrl}
            required
            placeholder="https://hooks.example.com/budget"
          />
        </div>
        <ProjectScopeField id="dest-project" bind:value={destProjectId} />
        <div class="form-field">
          <label for="dest-secret">Signing secret</label><input
            id="dest-secret"
            bind:value={destSecret}
            autocomplete="off"
            placeholder="Optional HMAC secret"
          />
        </div>
      </div>
      <div class="form-actions">
        <button
          class="button button-primary"
          type="submit"
          disabled={destBusy || !destName.trim() || !destUrl.trim()}
          >{destBusy ? 'Creating…' : 'Create destination'}</button
        >
      </div>
    </form>
  {/if}

  {#if destinations.isPending}<span role="status">Loading destinations…</span>
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
            ><th scope="col">Name</th><th scope="col">URL</th><th scope="col"
              >Project</th
            ><th scope="col">Enabled</th>{#if canManage}<th scope="col"
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
                    bind:value={editDestName}
                    required
                  /></td
                ><td
                  ><input
                    aria-label="Webhook URL"
                    type="url"
                    bind:value={editDestUrl}
                    required
                  /></td
                ><td>{destination.project_name ?? 'Installation-wide'}</td><td
                  >{destination.enabled ? 'Yes' : 'No'}</td
                ><td
                  ><input
                    aria-label="New signing secret"
                    bind:value={editDestSecret}
                    placeholder="Keep current"
                  /><button
                    class="text-button"
                    type="button"
                    disabled={editDestBusy}
                    onclick={() => saveDestination(destination)}
                    >{editDestBusy ? 'Saving…' : 'Save'}</button
                  ><button
                    class="text-button"
                    type="button"
                    disabled={editDestBusy}
                    onclick={() => (editDestId = '')}>Cancel</button
                  ></td
                ></tr
              >
            {:else}
              <tr
                ><td><strong>{destination.name}</strong></td><td
                  ><span class="mono">{destination.url}</span></td
                ><td>{destination.project_name ?? 'Installation-wide'}</td><td
                  >{destination.enabled ? 'Yes' : 'No'}</td
                >{#if canManage}<td
                    ><button
                      class="text-button"
                      type="button"
                      onclick={() => startDestEdit(destination)}>Edit</button
                    ><button
                      class="text-button"
                      type="button"
                      onclick={() => toggleDestination(destination)}
                      >{destination.enabled ? 'Disable' : 'Enable'}</button
                    ></td
                  >{/if}</tr
              >
            {/if}
          {/each}
        </tbody>
      </table>
    </div>
  {/if}

  <h3>Alert rules</h3>
  {#if canManage}
    <form class="create-form" onsubmit={submitRule}>
      <div class="form-grid">
        <div class="form-field">
          <label for="rule-name">Rule name</label><input
            id="rule-name"
            bind:value={ruleName}
            required
          />
        </div>
        <ProjectScopeField id="rule-project" bind:value={ruleProjectId} />
        <div class="form-field">
          <label for="rule-subject-kind">Subject</label><select
            id="rule-subject-kind"
            bind:value={ruleSubjectKind}
            ><option value="api_key">API key</option><option
              value="budget_group">Budget group</option
            ></select
          >
        </div>
        <div class="form-field">
          <label for="rule-subject">
            {ruleSubjectKind === 'api_key' ? 'API key' : 'Budget group'}</label
          ><select id="rule-subject" bind:value={ruleSubjectId} required
            ><option value="" disabled>Choose a subject</option
            >{#each subjectOptions as subject (subject.id)}<option
                value={subject.id}>{subject.name}</option
              >{/each}</select
          >
        </div>
        <div class="form-field">
          <label for="rule-window">Window</label><select
            id="rule-window"
            bind:value={ruleWindow}
            ><option value="day">UTC day</option><option value="month"
              >UTC month</option
            ></select
          >
        </div>
        <div class="form-field">
          <label for="rule-threshold">Threshold %</label><input
            id="rule-threshold"
            inputmode="numeric"
            bind:value={ruleThreshold}
            required
          />
        </div>
        <div class="form-field">
          <label for="rule-destination">Destination</label><select
            id="rule-destination"
            bind:value={ruleDestinationId}
            required
            ><option value="" disabled>Choose a destination</option
            >{#each destinationOptions as destination (destination.id)}<option
                value={destination.id}>{destination.name}</option
              >{/each}</select
          >
        </div>
      </div>
      <div class="form-actions">
        <button
          class="button button-primary"
          type="submit"
          disabled={ruleBusy ||
            !ruleName.trim() ||
            !ruleSubjectId ||
            !ruleDestinationId}>{ruleBusy ? 'Creating…' : 'Create rule'}</button
        >
      </div>
    </form>
  {/if}

  {#if rules.isPending}<span role="status">Loading alert rules…</span>
  {:else if rules.isError}<span class="inline-problem" role="alert"
      >Alert rules are unavailable.
      <button class="text-button" type="button" onclick={() => rules.refetch()}
        >Retry</button
      ></span
    >
  {:else if !(rules.data ?? []).length}<p class="section-help">
      No alert rules yet.
    </p>
  {:else}
    <div class="table-scroll">
      <table class="data-table">
        <thead
          ><tr
            ><th scope="col">Name</th><th scope="col">Subject</th><th
              scope="col">Window</th
            ><th scope="col">Threshold</th><th scope="col">Destination</th><th
              scope="col">Enabled</th
            >{#if canManage}<th scope="col"
                ><span class="sr-only">Actions</span></th
              >{/if}</tr
          ></thead
        >
        <tbody>
          {#each rules.data ?? [] as rule (rule.id)}
            <tr
              ><td
                ><strong>{rule.name}</strong><br /><small
                  >{rule.project_name ?? 'Installation-wide'}</small
                ></td
              ><td>{subjectLabel(rule)}</td><td
                >{rule.window_kind === 'day' ? 'UTC day' : 'UTC month'}</td
              ><td>{rule.threshold_percent}%</td><td>{rule.destination_name}</td
              ><td>{rule.enabled ? 'Yes' : 'No'}</td>{#if canManage}<td
                  ><button
                    class="text-button"
                    type="button"
                    onclick={() => toggleRule(rule)}
                    >{rule.enabled ? 'Disable' : 'Enable'}</button
                  ></td
                >{/if}</tr
            >
          {/each}
        </tbody>
      </table>
    </div>
  {/if}

  <h3>Deliveries</h3>
  {#if deliveries.isPending}<span role="status">Loading deliveries…</span>
  {:else if deliveries.isError}<span class="inline-problem" role="alert"
      >Deliveries are unavailable.
      <button
        class="text-button"
        type="button"
        onclick={() => deliveries.refetch()}>Retry</button
      ></span
    >
  {:else if !(deliveries.data ?? []).length}<p class="section-help">
      No deliveries recorded yet.
    </p>
  {:else}
    <div class="table-scroll">
      <table class="data-table">
        <thead
          ><tr
            ><th scope="col">Rule</th><th scope="col">Window</th><th scope="col"
              >Threshold</th
            ><th scope="col">Accrued</th><th scope="col">Limit</th><th
              scope="col">Status</th
            ><th scope="col">Attempts</th><th scope="col">Last error</th><th
              scope="col">Last attempt</th
            ></tr
          ></thead
        >
        <tbody>
          {#each deliveries.data ?? [] as delivery (delivery.id)}
            <tr
              ><td>{delivery.rule_name}</td><td
                ><span class="mono">{delivery.window_id}</span></td
              ><td>{delivery.threshold_percent}%</td><td
                ><span class="mono"
                  >{delivery.accrued}{delivery.currency
                    ? ` ${delivery.currency}`
                    : ''}</span
                ></td
              ><td
                ><span class="mono"
                  >{delivery.limit}{delivery.currency
                    ? ` ${delivery.currency}`
                    : ''}</span
                ></td
              ><td
                ><span class="badge" class:danger={delivery.status === 'failed'}
                  >{delivery.status}</span
                ></td
              ><td>{delivery.attempts}</td><td
                >{delivery.last_error_code ?? '—'}</td
              ><td
                >{delivery.last_attempt_at
                  ? formatDate(delivery.last_attempt_at)
                  : '—'}</td
              ></tr
            >
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

<style>
  .notifications-panel {
    display: grid;
    gap: 1rem;
    margin-top: 1.5rem;
    padding: 1.5rem;
  }
  .notifications-panel h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .notifications-panel h3 {
    margin: 0;
    font-size: 0.9rem;
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
  .badge.danger {
    color: var(--danger);
  }
</style>
