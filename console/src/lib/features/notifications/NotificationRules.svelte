<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { notificationKeys } from './notificationKeys';
  import { canChangeNotification } from './presentation';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction, useMutation } from '#lib/editing/action.svelte.js';
  import QueryView from '#lib/components/QueryView.svelte';
  import NotificationRuleDialog from './NotificationRuleDialog.svelte';
  import NotificationRuleEditRow from './NotificationRuleEditRow.svelte';
  import { listApiKeys } from '#lib/features/access/index.js';
  import { apiKeyKeys } from '#lib/features/access/index.js';
  import { listBudgetGroups } from '#lib/features/budgets/index.js';
  import { budgetGroupKeys } from '#lib/features/budgets/index.js';
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
  const canChange = (rule: NotificationRule) =>
    canChangeNotification(rule.project_id, {
      project: canManage,
      installation: installationWide
    });
  let creating = $state(false);
  const create = useMutation();
  const edit = useAction();
  const toggle = useAction();
  const problem = $derived(create.error || edit.error || toggle.error);
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
  const ruleProjectScoped = $derived(
    notificationEvents[ruleEvent].scope === 'project'
  );

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
      create.busy ||
      !draft.name.trim() ||
      ('subjectId' in draft && !draft.subjectId) ||
      !draft.destinationId
    )
      return;
    const parsed = createRuleInput(draft);
    if (parsed.kind === 'invalid') {
      create.error = parsed.message;
      return;
    }
    await create.submit(
      parsed.input,
      (input, requestId) => createNotificationRule(input, requestId),
      async () => {
        draft = resetRuleDraft(draft);
        creating = false;
        notify('Rule created');
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
            ? `Rule created, but refreshing failed. ${errorMessage(cause)}`
            : errorMessage(cause, 'The rule could not be created.')
      }
    );
  }

  let editRuleId = $state('');
  let editingRule = $state.raw<NotificationRule | null>(null);
  let editRuleName = $state('');
  let editRuleDestinationId = $state('');

  let editConfiguration = $state<RuleConfigurationDraft>(
    newRuleConfiguration('budget.threshold')
  );

  usePrincipalLifetime(() => {
    draft = newRuleDraft(ruleEvent);
    editRuleId = editRuleName = editRuleDestinationId = '';
    editingRule = null;
    editConfiguration = newRuleConfiguration('budget.threshold');
  });

  function startRuleEdit(rule: NotificationRule) {
    if (edit.busy || !canChange(rule)) return;
    editingRule = rule;
    editRuleId = rule.id;
    editRuleName = rule.name;
    editRuleDestinationId = rule.destination_id;
    editConfiguration = newRuleConfiguration(
      rule.event,
      rule.configuration ?? {}
    );
    edit.error = '';
  }

  async function saveRule(rule: NotificationRule) {
    if (
      !canChange(rule) ||
      edit.busy ||
      !editRuleName.trim() ||
      !editRuleDestinationId
    )
      return;
    const current = editingRule;
    if (!current || current.id !== rule.id) return;
    const parsed = parseRuleConfiguration(editConfiguration);
    if (parsed.kind === 'invalid') {
      edit.error = parsed.message;
      return;
    }
    await edit.run(
      () =>
        updateNotificationRule(current, {
          name: editRuleName.trim(),
          destination_id: editRuleDestinationId,
          configuration: parsed.configuration
        }),
      async () => {
        editRuleId = '';
        editingRule = null;
        notify('Rule updated.');
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
            ? `Rule updated, but refreshing failed. ${errorMessage(cause)}`
            : errorMessage(cause, 'The rule could not be updated.')
      }
    );
  }

  async function toggleRule(rule: NotificationRule) {
    if (!canChange(rule)) return;
    await toggle.run(
      () => updateNotificationRule(rule, { enabled: !rule.enabled }),
      () =>
        queryClient.invalidateQueries({
          queryKey: notificationKeys.root
        }),
      {
        failure: (cause) =>
          errorMessage(cause, 'The rule could not be updated.')
      }
    );
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

{#if problem && !creating}<Callout tone="danger">{problem}</Callout>{/if}

{#if canManage}<div class="toolbar">
    <p class="section-help">
      When a budget is close to its limit, a key expires, or deliveries fail.
    </p>
    <div class="page-actions">
      <button
        class="button button-primary"
        type="button"
        onclick={() => {
          create.error = '';
          creating = true;
        }}>New rule</button
      >
    </div>
  </div>{/if}
{#if creating}
  <NotificationRuleDialog
    bind:draft
    busy={create.busy}
    error={problem}
    {installationWide}
    {subjectOptions}
    {destinationOptions}
    onClose={() => (creating = false)}
    onSubmit={submitRule}
  />
{/if}

<QueryView
  query={rules}
  loading="Loading rules…"
  failure="Rules are unavailable."
  emptyWhen={!(rules.data ?? []).length}
>
  {#snippet empty()}
    <EmptyState title="No alert rules yet"
      >Rules decide which events send an alert, and to which destination.</EmptyState
    >
  {/snippet}
  <ScrollTable label="Notification rules">
    <table class="data-table">
      <thead
        ><tr
          ><th scope="col">Name</th><th scope="col">Event</th><th scope="col"
            >Subject</th
          ><th scope="col">Window</th><th scope="col">Configuration</th><th
            scope="col">Destination</th
          ><th scope="col">Enabled</th>{#if canManage || installationWide}<th
              scope="col"><span class="sr-only">Actions</span></th
            >{/if}</tr
        ></thead
      >
      <tbody>
        {#each rules.data ?? [] as rule (rule.id)}
          {#if editRuleId === rule.id}
            <NotificationRuleEditRow
              {rule}
              bind:name={editRuleName}
              bind:destinationId={editRuleDestinationId}
              bind:configuration={editConfiguration}
              destinations={destinations.data ?? []}
              busy={edit.busy}
              {editingRule}
              onSave={() => saveRule(rule)}
              onCancel={() => {
                editRuleId = '';
                editingRule = null;
              }}
              onSilenceSaved={async (value) => {
                if (editRuleId === value.id) editingRule = value;
                await queryClient.invalidateQueries({
                  queryKey: notificationKeys.rules()
                });
              }}
              {subjectLabel}
              {windowLabel}
            />
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
              >{#if canManage || installationWide}<td
                  >{#if canChange(rule)}<button
                      class="text-button"
                      type="button"
                      disabled={edit.busy}
                      onclick={() => startRuleEdit(rule)}>Edit</button
                    ><button
                      class="text-button"
                      type="button"
                      disabled={toggle.busy}
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
                      />{/if}{/if}</td
                >{/if}</tr
            >
          {/if}
        {/each}
      </tbody>
    </table>
  </ScrollTable>
</QueryView>
