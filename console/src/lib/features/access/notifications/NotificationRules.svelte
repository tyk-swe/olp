<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import ProjectScopeField from '#lib/features/access/projects/ProjectScopeField.svelte';
  import { notificationKeys } from './notificationKeys';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import { listApiKeys } from '#lib/features/access/api-keys/api.js';
  import { apiKeyKeys } from '#lib/features/access/api-keys/apiKeyKeys.js';
  import { listBudgetGroups } from '#lib/features/access/budget-groups/api.js';
  import { budgetGroupKeys } from '#lib/features/access/budget-groups/budgetGroupKeys.js';
  import {
    createNotificationRule,
    deleteNotificationRule,
    listNotificationRules,
    listNotificationDestinations,
    updateNotificationRule,
    type NotificationRule
  } from './api';
  import {
    notificationEvents,
    newRuleDraft,
    resetRuleDraft,
    createRuleInput,
    type RuleDraft,
    newRuleConfiguration,
    parseRuleConfiguration,
    type RuleConfigurationDraft
  } from './ruleDraft';
  const queryClient = useQueryClient();
  let {
    canManage,
    installationWide,
    canDelete
  }: { canManage: boolean; installationWide: boolean; canDelete: boolean } =
    $props();
  let creating = $state(false);
  let error = $state('');
  let notice = $state('');
  const ruleIntent = useMutationIntent();
  const destinations = createQuery(() => ({
    queryKey: notificationKeys.destinations(),
    queryFn: ({ signal }) => listNotificationDestinations(signal)
  }));
  const rules = createQuery(() => ({
    queryKey: notificationKeys.rules(),
    queryFn: ({ signal }) => listNotificationRules(signal)
  }));
  const apiKeys = createQuery(() => ({
    queryKey: apiKeyKeys.list(),
    queryFn: ({ signal }) => listApiKeys(signal)
  }));
  const groups = createQuery(() => ({
    queryKey: budgetGroupKeys.list(),
    queryFn: ({ signal }) => listBudgetGroups(signal)
  }));

  let draft = $state<RuleDraft>(newRuleDraft('budget.threshold'));
  const ruleEvent = $derived(draft.event);
  const watchesBudget = $derived(ruleEvent === 'budget.threshold');
  const watchesKey = $derived(ruleEvent === 'key.expiring');
  const genericRule = $derived(
    !watchesBudget && ruleEvent !== 'provider.grant.lapsed'
  );
  const ruleProjectScoped = $derived(
    notificationEvents[ruleEvent].scope === 'project'
  );
  let ruleBusy = $state(false);

  const subjectOptions = $derived(
    !(
      draft.event === 'budget.threshold' && draft.subjectKind === 'budget_group'
    )
      ? (apiKeys.data ?? []).filter(
          (key) => (key.project_id ?? null) === (draft.projectId || null)
        )
      : (groups.data ?? []).filter(
          (group) => (group.project_id ?? null) === (draft.projectId || null)
        )
  );
  const destinationOptions = $derived(
    (destinations.data ?? []).filter(
      (destination) =>
        (destination.project_id ?? null) ===
        (ruleProjectScoped ? draft.projectId || null : null)
    )
  );

  $effect(() => {
    const current = draft;
    if (
      'subjectId' in current &&
      current.subjectId &&
      !subjectOptions.some((subject) => subject.id === current.subjectId)
    )
      draft = { ...current, subjectId: '' };

    if (
      draft.destinationId &&
      !destinationOptions.some(
        (destination) => destination.id === draft.destinationId
      )
    )
      draft.destinationId = '';
  });

  async function submitRule(event: SubmitEvent) {
    event.preventDefault();
    if (
      !canManage ||
      ruleBusy ||
      !draft.name.trim() ||
      ('subjectId' in draft && !draft.subjectId) ||
      !draft.destinationId
    )
      return;
    const parsed = createRuleInput(draft);
    if (parsed.kind === 'invalid') {
      error = parsed.message;
      return;
    }
    ruleBusy = true;
    const isCurrent = captureOwner();
    error = notice = '';
    let committed = false;
    try {
      const submission = ruleIntent.prepare(parsed.input);
      await createNotificationRule(submission.input, submission.requestId);
      if (!isCurrent()) return;
      ruleIntent.reset();
      committed = true;
      draft = resetRuleDraft(draft);
      creating = false;
      notify('Rule created');
      await queryClient.invalidateQueries(
        {
          queryKey: notificationKeys.root
        },
        { throwOnError: true }
      );
    } catch (cause) {
      if (!isCurrent()) return;
      error = committed
        ? `Rule created, but refreshing failed. ${errorMessage(cause)}`
        : errorMessage(cause, 'The rule could not be created.');
    } finally {
      if (isCurrent()) ruleBusy = false;
    }
  }

  let editRuleId = $state('');
  let editingRule = $state.raw<NotificationRule | null>(null);
  let editRuleName = $state('');
  let editRuleDestinationId = $state('');
  let editRuleBusy = $state(false);

  let editConfiguration = $state<RuleConfigurationDraft>(
    newRuleConfiguration('budget.threshold')
  );

  const captureOwner = usePrincipalLifetime(() => {
    draft = newRuleDraft(ruleEvent);
    editRuleId = editRuleName = editRuleDestinationId = '';
    editingRule = null;
    editConfiguration = newRuleConfiguration('budget.threshold');
    ruleBusy = editRuleBusy = false;
    error = notice = '';
  });

  function startRuleEdit(rule: NotificationRule) {
    if (editRuleBusy) return;
    editingRule = rule;
    editRuleId = rule.id;
    editRuleName = rule.name;
    editRuleDestinationId = rule.destination_id;
    editConfiguration = newRuleConfiguration(
      rule.event,
      rule.configuration ?? {}
    );
    error = '';
  }

  async function saveRule(rule: NotificationRule) {
    if (
      !canManage ||
      editRuleBusy ||
      !editRuleName.trim() ||
      !editRuleDestinationId
    )
      return;
    const current = editingRule;
    if (!current || current.id !== rule.id) return;
    editRuleBusy = true;
    const isCurrent = captureOwner();
    error = notice = '';
    const parsed = parseRuleConfiguration(editConfiguration);
    if (parsed.kind === 'invalid') {
      error = parsed.message;
      editRuleBusy = false;
      return;
    }
    let committed = false;
    try {
      await updateNotificationRule(current, {
        name: editRuleName.trim(),
        destination_id: editRuleDestinationId,
        configuration: parsed.configuration
      });
      committed = true;
      if (!isCurrent()) return;
      editRuleId = '';
      editingRule = null;
      notice = 'Rule updated.';
      await queryClient.invalidateQueries(
        {
          queryKey: notificationKeys.root
        },
        { throwOnError: true }
      );
    } catch (cause) {
      if (!isCurrent()) return;
      error = committed
        ? `Rule updated, but refreshing failed. ${errorMessage(cause)}`
        : errorMessage(cause, 'The rule could not be updated.');
    } finally {
      if (isCurrent()) editRuleBusy = false;
    }
  }

  async function toggleRule(rule: NotificationRule) {
    if (!canManage) return;
    const isCurrent = captureOwner();
    error = notice = '';
    try {
      await updateNotificationRule(rule, { enabled: !rule.enabled });
      if (!isCurrent()) return;
      await queryClient.invalidateQueries({
        queryKey: notificationKeys.root
      });
    } catch (cause) {
      if (!isCurrent()) return;
      error = errorMessage(cause, 'The rule could not be updated.');
    }
  }

  function subjectLabel(rule: NotificationRule) {
    if (rule.event === 'provider.grant.lapsed') return 'Every provider';
    if (rule.subject_id == null) return '—';
    return `${rule.subject_kind === 'api_key' ? 'API key' : 'Budget group'} · ${rule.subject_name ?? rule.subject_id}`;
  }

  function configurationLabel(rule: NotificationRule) {
    const parts = Object.entries(rule.configuration ?? {}).map(
      ([key, value]) => `${key}=${String(value)}`
    );
    return parts.length ? parts.join(', ') : '—';
  }

  // Budget windows follow the installation's budget time zone, which may
  // change, so the labels name no zone.
  function windowLabel(windowKind: NotificationRule['window_kind']) {
    if (!windowKind) return '—';
    return windowKind === 'day'
      ? 'Budget day'
      : windowKind === 'week'
        ? 'Budget week'
        : 'Budget month';
  }
</script>

{#if error && !creating}<Callout tone="danger">{error}</Callout>{/if}
{#if notice}<p class="section-help" role="status">{notice}</p>{/if}
{#if canManage}<div class="toolbar">
    <span></span>
    <div class="page-actions">
      <button
        class="button button-primary"
        type="button"
        onclick={() => {
          error = '';
          creating = true;
        }}>New rule</button
      >
    </div>
  </div>{/if}
{#if creating}
  <Dialog
    title="New alert rule"
    description="Send an alert to a destination when something happens, such as a budget threshold."
    placement="side"
    onClose={() => (creating = false)}
  >
    {#if error}<Callout tone="danger">{error}</Callout>{/if}
    <form class="create-form" onsubmit={submitRule}>
      <fieldset class="submission-fields" disabled={ruleBusy}>
        <div class="form-grid">
          <div class="form-field">
            <label for="rule-name">Rule name</label><input
              id="rule-name"
              bind:value={draft.name}
              required
            />
          </div>
          <div class="form-field">
            <label for="rule-event">Event</label><select
              id="rule-event"
              value={ruleEvent}
              onchange={(event) => {
                const selected = Object.values(notificationEvents).find(
                  (definition) => definition.event === event.currentTarget.value
                );
                if (selected) draft = newRuleDraft(selected.event, draft);
              }}
              >{#each Object.values(notificationEvents) as definition (definition.event)}<option
                  value={definition.event}
                  disabled={!installationWide && definition.scope !== 'project'}
                  >{definition.label}</option
                >{/each}</select
            >
          </div>
          {#if ruleProjectScoped}
            <ProjectScopeField
              id="rule-project"
              bind:value={draft.projectId}
              unassigned={installationWide}
            />
          {/if}
          {#if draft.event === 'budget.threshold' || draft.event === 'key.expiring'}
            <div class="form-field">
              <label for="rule-subject-kind">Subject</label><select
                id="rule-subject-kind"
                disabled={watchesKey}
                bind:value={draft.subjectKind}
                ><option value="api_key">API key</option><option
                  value="budget_group">Budget group</option
                ></select
              >
            </div>
            <div class="form-field">
              <label for="rule-subject">
                {draft.subjectKind === 'api_key'
                  ? 'API key'
                  : 'Budget group'}</label
              ><select id="rule-subject" bind:value={draft.subjectId} required
                ><option value="" disabled>Choose a subject</option
                >{#each subjectOptions as subject (subject.id)}<option
                    value={subject.id}>{subject.name}</option
                  >{/each}</select
              >
            </div>
            {#if draft.event === 'budget.threshold'}
              <div class="form-field">
                <label for="rule-window">Window</label><select
                  id="rule-window"
                  bind:value={draft.window}
                  ><option value="day">Budget day</option><option value="week"
                    >Budget week</option
                  ><option value="month">Budget month</option></select
                >
              </div>
              <div class="form-field">
                <label for="rule-threshold">Threshold %</label><input
                  id="rule-threshold"
                  inputmode="numeric"
                  bind:value={draft.threshold}
                  required
                />
              </div>
            {/if}
          {/if}
          {#each notificationEvents[ruleEvent].fields as field (field.key)}
            <div class="form-field">
              <label for="rule-config-{field.key}">{field.label}</label><input
                id="rule-config-{field.key}"
                inputmode={field.inputmode}
                value={draft.values[field.key] ?? ''}
                oninput={(e) =>
                  (draft.values = {
                    ...draft.values,
                    [field.key]: e.currentTarget.value
                  })}
                placeholder="Default"
              />
            </div>
          {/each}
          {#if draft.event === 'route.latency'}
            <div class="form-field">
              <label for="rule-metric">Metric</label><select
                id="rule-metric"
                bind:value={draft.metric}
                ><option value="ttft">Time to first token</option><option
                  value="latency">Total latency</option
                ></select
              >
            </div>
          {/if}
          {#if draft.event === 'report.spend'}
            <div class="form-field">
              <label for="rule-period">Period</label><select
                id="rule-period"
                bind:value={draft.period}
                ><option value="daily">Daily</option><option value="weekly"
                  >Weekly</option
                ><option value="monthly">Monthly</option></select
              >
            </div>
          {/if}
          <div class="form-field">
            <label for="rule-destination">Destination</label><select
              id="rule-destination"
              bind:value={draft.destinationId}
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
              !draft.name.trim() ||
              ('subjectId' in draft && !draft.subjectId) ||
              !draft.destinationId}
            >{ruleBusy ? 'Creating…' : 'Create rule'}</button
          >
        </div>
        <p class="section-help">
          {#if draft.event === 'budget.threshold'}A delivery is sent when a
            subject's accrued spend crosses the threshold within the window.{:else if watchesKey}Sent
            once when the key expires or reaches its rotation date within the
            lead time.{:else if genericRule}Metadata-only trigger and recovery
            deliveries; cooldown repeats share the incident key.{:else}A grant
            lapses when its provider plugin can no longer refresh it.{/if}
        </p>
      </fieldset>
    </form>
  </Dialog>
{/if}

{#if rules.isPending}<span class="inline-status" role="status"
    >Loading rules…</span
  >
{:else if rules.isError}<span class="inline-problem" role="alert"
    >Rules are unavailable.
    <button class="text-button" type="button" onclick={() => rules.refetch()}
      >Retry</button
    ></span
  >
{:else if !(rules.data ?? []).length}<p class="section-help">No rules yet.</p>
{:else}
  <div class="table-scroll">
    <table class="data-table">
      <thead
        ><tr
          ><th scope="col">Name</th><th scope="col">Event</th><th scope="col"
            >Subject</th
          ><th scope="col">Window</th><th scope="col">Configuration</th><th
            scope="col">Destination</th
          ><th scope="col">Enabled</th>{#if canManage}<th scope="col"
              ><span class="sr-only">Actions</span></th
            >{/if}</tr
        ></thead
      >
      <tbody>
        {#each rules.data ?? [] as rule (rule.id)}
          {#if editRuleId === rule.id}
            <tr
              ><td
                ><input
                  aria-label="Rule name"
                  bind:value={editRuleName}
                  disabled={editRuleBusy}
                  required
                /><br /><small>{rule.project_name ?? 'Installation-wide'}</small
                ></td
              ><td>{notificationEvents[rule.event].label}</td><td
                >{subjectLabel(rule)}</td
              ><td>{windowLabel(rule.window_kind)}</td><td
                ><div class="form-grid">
                  {#each notificationEvents[rule.event].fields as field (field.key)}
                    <input
                      aria-label={field.label}
                      inputmode={field.inputmode}
                      value={editConfiguration.values[field.key] ?? ''}
                      disabled={editRuleBusy}
                      oninput={(e) =>
                        (editConfiguration.values = {
                          ...editConfiguration.values,
                          [field.key]: e.currentTarget.value
                        })}
                      placeholder={`${field.key} (default)`}
                    />
                  {/each}
                  {#if editConfiguration.event === 'route.latency'}<select
                      aria-label="Metric"
                      bind:value={editConfiguration.metric}
                      disabled={editRuleBusy}
                      ><option value="ttft">Time to first token</option><option
                        value="latency">Total latency</option
                      ></select
                    >{/if}
                  {#if editConfiguration.event === 'report.spend'}<select
                      aria-label="Period"
                      bind:value={editConfiguration.period}
                      disabled={editRuleBusy}
                      ><option value="daily">Daily</option><option
                        value="weekly">Weekly</option
                      ><option value="monthly">Monthly</option></select
                    >{/if}
                </div></td
              ><td
                ><select
                  aria-label="Destination"
                  bind:value={editRuleDestinationId}
                  disabled={editRuleBusy}
                  >{#each (destinations.data ?? []).filter((d) => (d.project_id ?? null) === (rule.project_id ?? null)) as destination (destination.id)}<option
                      value={destination.id}>{destination.name}</option
                    >{/each}</select
                ></td
              ><td>{rule.enabled ? 'Yes' : 'No'}</td><td
                ><button
                  class="text-button"
                  type="button"
                  disabled={editRuleBusy}
                  onclick={() => saveRule(rule)}
                  >{editRuleBusy ? 'Saving…' : 'Save'}</button
                ><button
                  class="text-button"
                  type="button"
                  disabled={editRuleBusy}
                  onclick={() => {
                    editRuleId = '';
                    editingRule = null;
                  }}>Cancel</button
                ></td
              ></tr
            >
          {:else}
            <tr
              ><td
                ><strong>{rule.name}</strong><br /><small
                  >{rule.project_name ?? 'Installation-wide'}</small
                ></td
              ><td>{notificationEvents[rule.event].label}</td><td
                >{subjectLabel(rule)}</td
              ><td>{windowLabel(rule.window_kind)}</td><td
                ><span class="mono">{configurationLabel(rule)}</span></td
              ><td>{rule.destination_name}</td><td
                >{rule.enabled ? 'Yes' : 'No'}</td
              >{#if canManage}<td
                  ><button
                    class="text-button"
                    type="button"
                    disabled={editRuleBusy}
                    onclick={() => startRuleEdit(rule)}>Edit</button
                  ><button
                    class="text-button"
                    type="button"
                    onclick={() => toggleRule(rule)}
                    >{rule.enabled ? 'Disable' : 'Enable'}</button
                  >
                  {#if canDelete}<ConfirmAction
                      danger
                      label="Delete rule"
                      description={`Delete ${rule.name}. Referenced resources cannot be deleted`}
                      identity={`${rule.id}:${rule.etag}`}
                      run={(requestId) =>
                        deleteNotificationRule(rule, requestId)}
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
