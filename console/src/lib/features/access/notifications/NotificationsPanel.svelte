<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { holds } from '$lib/features/access/session/authorization';
  import { useServiceCapabilities } from '$lib/features/access/session/serviceCapabilities.svelte';
  import ProjectScopeField from '$lib/features/access/projects/ProjectScopeField.svelte';
  import { listApiKeys } from '$lib/features/access/api-keys/api';
  import { apiKeyKeys } from '$lib/features/access/api-keys/apiKeyKeys';
  import { listBudgetGroups } from '$lib/features/access/budget-groups/api';
  import { budgetGroupKeys } from '$lib/features/access/budget-groups/budgetGroupKeys';
  import {
    createNotificationDestination,
    createNotificationRule,
    listNotificationDeliveries,
    listNotificationDestinations,
    listNotificationRules,
    updateNotificationDestination,
    updateNotificationRule,
    type NotificationDestination,
    type NotificationEvent,
    type NotificationRule
  } from '$lib/features/access/notifications/api';
  import { notificationKeys } from '$lib/features/access/notifications/notificationKeys';
  import { formatDate } from '$lib/format';

  const services = useServiceCapabilities();
  const queryClient = useQueryClient();
  const access = useRole();
  const canManage = $derived(access.can('api_keys.manage'));
  // Installation-wide notifications are installation settings.
  const installationWide = $derived(holds(access.user, 'settings'));

  const destinations = createQuery(() => ({
    queryKey: notificationKeys.destinations(),
    queryFn: ({ signal }) => listNotificationDestinations(signal)
  }));
  const rules = createQuery(() => ({
    queryKey: notificationKeys.rules(),
    queryFn: ({ signal }) => listNotificationRules(signal)
  }));
  const deliveries = createQuery(() => ({
    queryKey: notificationKeys.deliveries(),
    queryFn: ({ signal }) => listNotificationDeliveries(undefined, signal)
  }));
  const apiKeys = createQuery(() => ({
    queryKey: apiKeyKeys.list(),
    queryFn: ({ signal }) => listApiKeys(signal)
  }));
  const groups = createQuery(() => ({
    queryKey: budgetGroupKeys.list(),
    queryFn: ({ signal }) => listBudgetGroups(signal)
  }));

  let error = $state('');
  let notice = $state('');

  type DestinationType = NonNullable<NotificationDestination['type']>;
  const destinationTypes: { value: DestinationType; label: string }[] = [
    { value: 'webhook', label: 'Webhook' },
    { value: 'slack', label: 'Slack' },
    { value: 'msteams', label: 'Microsoft Teams' },
    { value: 'discord', label: 'Discord' },
    { value: 'pagerduty', label: 'PagerDuty' },
    { value: 'email', label: 'Email (SMTP)' }
  ];
  const isChat = (type: DestinationType) =>
    type === 'slack' || type === 'msteams' || type === 'discord';
  const secretRequired = (type: DestinationType) =>
    isChat(type) || type === 'pagerduty';

  let destName = $state('');
  let destType = $state<DestinationType>('webhook');
  let destUrl = $state('');
  let destWebhookUrl = $state('');
  let destRoutingKey = $state('');
  let destEmailFrom = $state('');
  let destEmailTo = $state('');
  let destEmailSubjectPrefix = $state('');
  let destEmailCa = $state('');
  let destEmailUsername = $state('');
  let destEmailPassword = $state('');
  let destProjectId = $state('');
  let destSecret = $state('');
  let destBusy = $state(false);

  function resetDestinationForm() {
    destName = '';
    destUrl = '';
    destWebhookUrl = '';
    destRoutingKey = '';
    destEmailFrom = '';
    destEmailTo = '';
    destEmailSubjectPrefix = '';
    destEmailCa = '';
    destEmailUsername = '';
    destEmailPassword = '';
    destSecret = '';
  }

  function destinationEmailConfiguration() {
    const configuration: Record<string, unknown> = {
      from: destEmailFrom.trim(),
      to: destEmailTo
        .split(',')
        .map((address) => address.trim())
        .filter(Boolean)
    };
    if (destEmailSubjectPrefix.trim())
      configuration.subject_prefix = destEmailSubjectPrefix.trim();
    if (destEmailCa.trim()) configuration.ca_certificate = destEmailCa.trim();
    return configuration;
  }

  function destinationCreateBody() {
    const name = destName.trim();
    const project_id = destProjectId || null;
    switch (destType) {
      case 'slack':
      case 'msteams':
      case 'discord':
        return {
          name,
          type: destType,
          url: destUrl.trim(),
          project_id,
          secret: { webhook_url: destWebhookUrl.trim() }
        };
      case 'pagerduty':
        return {
          name,
          type: destType,
          url: destUrl.trim(),
          project_id,
          secret: { routing_key: destRoutingKey.trim() }
        };
      case 'email': {
        const secret =
          destEmailUsername.trim() || destEmailPassword
            ? {
                username: destEmailUsername.trim(),
                password: destEmailPassword
              }
            : null;
        return {
          name,
          type: destType,
          url: destUrl.trim(),
          project_id,
          configuration: destinationEmailConfiguration(),
          secret
        };
      }
      default:
        return {
          name,
          type: 'webhook' as const,
          url: destUrl.trim(),
          project_id,
          secret: destSecret.trim() || null
        };
    }
  }

  async function submitDestination(event: SubmitEvent) {
    event.preventDefault();
    if (!canManage || destBusy || !destName.trim() || !destUrl.trim()) return;
    destBusy = true;
    error = notice = '';
    try {
      await createNotificationDestination(destinationCreateBody());
      resetDestinationForm();
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

  let editDestId = $state('');
  let editDestName = $state('');
  let editDestUrl = $state('');
  let editDestSecret = $state('');
  let editDestClear = $state(false);
  let editDestBusy = $state(false);

  let editEmailFrom = $state('');
  let editEmailTo = $state('');
  let editEmailPrefix = $state('');
  let editEmailCa = $state('');

  function startDestEdit(destination: NotificationDestination) {
    editDestId = destination.id;
    editDestName = destination.name;
    editDestUrl = destination.url;
    editDestSecret = '';
    editDestClear = false;
    const config = (destination.configuration ?? {}) as Record<string, unknown>;
    editEmailFrom = String(config.from ?? '');
    editEmailTo = Array.isArray(config.to)
      ? (config.to as string[]).join(', ')
      : '';
    editEmailPrefix = String(config.subject_prefix ?? '');
    editEmailCa = String(config.ca_certificate ?? '');
    error = '';
  }

  function editSecretValue(destination: NotificationDestination) {
    const type = destination.type ?? 'webhook';
    const raw = editDestSecret.trim();
    if (isChat(type)) return { webhook_url: raw };
    if (type === 'pagerduty') return { routing_key: raw };
    if (type === 'email') return JSON.parse(raw) as Record<string, string>;
    return raw;
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
      const isEmail = (destination.type ?? 'webhook') === 'email';
      await updateNotificationDestination(destination, {
        name: editDestName.trim(),
        url: editDestUrl.trim(),
        ...(isEmail
          ? {
              configuration: {
                from: editEmailFrom.trim(),
                to: editEmailTo
                  .split(',')
                  .map((address) => address.trim())
                  .filter(Boolean),
                ...(editEmailPrefix.trim()
                  ? { subject_prefix: editEmailPrefix.trim() }
                  : {}),
                ...(editEmailCa.trim()
                  ? { ca_certificate: editEmailCa.trim() }
                  : {})
              }
            }
          : {}),
        ...(editDestClear
          ? { secret: null }
          : editDestSecret.trim()
            ? { secret: editSecretValue(destination) }
            : {})
      });
      editDestId = '';
      editDestSecret = '';
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

  const eventLabels: Record<NotificationEvent, string> = {
    'budget.threshold': 'Budget threshold',
    'budget.exhausted': 'Budget exhausted',
    'provider.grant.lapsed': 'Grant lapsed',
    'provider.circuit.open': 'Provider circuit opened',
    'provider.circuit.closed': 'Provider circuit closed',
    'provider.error_rate': 'Provider error rate',
    'provider.credential.failing': 'Provider credential failing',
    'route.latency': 'Route latency',
    'model.retirement': 'Model retirement',
    'runtime.install_failed': 'Runtime install failed',
    'worker.stale': 'Worker stale',
    'key.expiring': 'Key expiry or rotation due',
    'report.spend': 'Spend report'
  };

  const projectScopedEvents = new Set<NotificationEvent>([
    'budget.threshold',
    'key.expiring',
    'budget.exhausted',
    'route.latency',
    'model.retirement',
    'report.spend'
  ]);

  const eventConfigFields: Record<
    string,
    { key: string; label: string; inputmode?: 'numeric' | 'decimal' }[]
  > = {
    'provider.error_rate': [
      { key: 'threshold', label: 'Error-rate threshold', inputmode: 'decimal' },
      {
        key: 'recovery_threshold',
        label: 'Recovery threshold',
        inputmode: 'decimal'
      },
      { key: 'window_seconds', label: 'Window seconds', inputmode: 'numeric' },
      {
        key: 'minimum_samples',
        label: 'Minimum samples',
        inputmode: 'numeric'
      },
      {
        key: 'cooldown_seconds',
        label: 'Cooldown seconds',
        inputmode: 'numeric'
      }
    ],
    'route.latency': [
      { key: 'threshold', label: 'Latency threshold ms', inputmode: 'decimal' },
      {
        key: 'recovery_threshold',
        label: 'Recovery threshold ms',
        inputmode: 'decimal'
      },
      { key: 'window_seconds', label: 'Window seconds', inputmode: 'numeric' },
      {
        key: 'minimum_samples',
        label: 'Minimum samples',
        inputmode: 'numeric'
      },
      {
        key: 'cooldown_seconds',
        label: 'Cooldown seconds',
        inputmode: 'numeric'
      }
    ],
    'provider.credential.failing': [
      {
        key: 'threshold',
        label: 'Failure count threshold',
        inputmode: 'decimal'
      },
      {
        key: 'recovery_threshold',
        label: 'Recovery threshold',
        inputmode: 'decimal'
      },
      { key: 'window_seconds', label: 'Window seconds', inputmode: 'numeric' },
      {
        key: 'minimum_samples',
        label: 'Minimum samples',
        inputmode: 'numeric'
      },
      {
        key: 'cooldown_seconds',
        label: 'Cooldown seconds',
        inputmode: 'numeric'
      }
    ],
    'model.retirement': [
      { key: 'lead_days', label: 'Lead days', inputmode: 'numeric' },
      {
        key: 'cooldown_seconds',
        label: 'Cooldown seconds',
        inputmode: 'numeric'
      }
    ],
    'runtime.install_failed': [
      {
        key: 'cooldown_seconds',
        label: 'Cooldown seconds',
        inputmode: 'numeric'
      }
    ],
    'worker.stale': [
      {
        key: 'cooldown_seconds',
        label: 'Cooldown seconds',
        inputmode: 'numeric'
      }
    ],
    'provider.circuit.open': [
      {
        key: 'cooldown_seconds',
        label: 'Cooldown seconds',
        inputmode: 'numeric'
      }
    ],
    'provider.circuit.closed': [
      {
        key: 'cooldown_seconds',
        label: 'Cooldown seconds',
        inputmode: 'numeric'
      }
    ],
    'budget.exhausted': [
      {
        key: 'cooldown_seconds',
        label: 'Cooldown seconds',
        inputmode: 'numeric'
      }
    ],
    'key.expiring': [
      {
        key: 'lead_time_seconds',
        label: 'Lead time seconds',
        inputmode: 'numeric'
      }
    ]
  };

  let ruleName = $state('');
  let ruleEvent = $state<NotificationEvent>('budget.threshold');
  const watchesBudget = $derived(ruleEvent === 'budget.threshold');
  const watchesKey = $derived(ruleEvent === 'key.expiring');
  const genericRule = $derived(
    !watchesBudget && ruleEvent !== 'provider.grant.lapsed'
  );
  const ruleProjectScoped = $derived(projectScopedEvents.has(ruleEvent));
  $effect(() => {
    if (watchesKey) ruleSubjectKind = 'api_key';
  });
  let ruleProjectId = $state('');
  let ruleSubjectKind = $state<'api_key' | 'budget_group'>('api_key');
  let ruleSubjectId = $state('');
  let ruleWindow = $state<'day' | 'week' | 'month'>('month');
  let ruleThreshold = $state('80');
  let ruleMetric = $state<'ttft' | 'latency'>('ttft');
  let rulePeriod = $state<'daily' | 'weekly' | 'monthly'>('daily');
  let ruleConfig = $state<Record<string, string>>({});
  let ruleDestinationId = $state('');
  let ruleBusy = $state(false);

  $effect(() => {
    void ruleEvent;
    ruleConfig = {};
  });

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
        (destination.project_id ?? null) ===
        (ruleProjectScoped ? ruleProjectId || null : null)
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

  function ruleConfiguration() {
    const configuration: Record<string, unknown> = {};
    for (const field of eventConfigFields[ruleEvent] ?? []) {
      const value = (ruleConfig[field.key] ?? '').trim();
      if (!value) continue;
      configuration[field.key] = /^(threshold|recovery_threshold)$/.test(
        field.key
      )
        ? value
        : /^\d+$/.test(value)
          ? Number(value)
          : value;
    }
    if (ruleEvent === 'route.latency') configuration.metric = ruleMetric;
    if (ruleEvent === 'report.spend') configuration.period = rulePeriod;
    if (
      ruleEvent === 'key.expiring' &&
      (ruleConfig.lead_time_seconds ?? '').trim()
    )
      configuration.lead_time_seconds = Number(ruleConfig.lead_time_seconds);
    return Object.keys(configuration).length ? configuration : undefined;
  }

  async function submitRule(event: SubmitEvent) {
    event.preventDefault();
    if (
      !canManage ||
      ruleBusy ||
      !ruleName.trim() ||
      ((watchesBudget || watchesKey) && !ruleSubjectId) ||
      !ruleDestinationId
    )
      return;
    const threshold = Number(ruleThreshold);
    if (
      watchesBudget &&
      (!Number.isInteger(threshold) || threshold < 1 || threshold > 100)
    ) {
      error = 'Threshold must be a whole percentage from 1 to 100.';
      return;
    }
    ruleBusy = true;
    error = notice = '';
    const configuration = ruleConfiguration();
    try {
      await createNotificationRule(
        watchesBudget
          ? {
              name: ruleName.trim(),
              event: ruleEvent,
              project_id: ruleProjectId || null,
              subject_kind: ruleSubjectKind,
              subject_id: ruleSubjectId,
              window_kind: ruleWindow,
              threshold_percent: threshold,
              destination_id: ruleDestinationId
            }
          : watchesKey
            ? {
                name: ruleName.trim(),
                event: ruleEvent,
                project_id: ruleProjectId || null,
                subject_kind: 'api_key',
                subject_id: ruleSubjectId,
                destination_id: ruleDestinationId,
                ...(configuration ? { configuration } : {})
              }
            : {
                name: ruleName.trim(),
                event: ruleEvent,
                destination_id: ruleDestinationId,
                ...(ruleProjectScoped
                  ? { project_id: ruleProjectId || null }
                  : {}),
                ...(configuration ? { configuration } : {})
              }
      );
      ruleName = '';
      ruleSubjectId = '';
      ruleDestinationId = '';
      ruleConfig = {};
      notice = 'Rule created.';
      await queryClient.invalidateQueries({
        queryKey: notificationKeys.root
      });
    } catch (cause) {
      error = errorMessage(cause, 'The rule could not be created.');
    } finally {
      ruleBusy = false;
    }
  }

  let editRuleId = $state('');
  let editRuleName = $state('');
  let editRuleDestinationId = $state('');
  let editRuleConfig = $state<Record<string, string>>({});
  let editRuleMetric = $state<'ttft' | 'latency'>('ttft');
  let editRulePeriod = $state<'daily' | 'weekly' | 'monthly'>('daily');
  let editRuleBusy = $state(false);

  function startRuleEdit(rule: NotificationRule) {
    editRuleId = rule.id;
    editRuleName = rule.name;
    editRuleDestinationId = rule.destination_id;
    const config = (rule.configuration ?? {}) as Record<string, unknown>;
    editRuleConfig = {};
    for (const field of eventConfigFields[rule.event] ?? []) {
      const value = config[field.key];
      if (value !== undefined && value !== null)
        editRuleConfig[field.key] = String(value);
    }
    if (typeof config.metric === 'string')
      editRuleMetric = config.metric === 'latency' ? 'latency' : 'ttft';
    if (typeof config.period === 'string')
      editRulePeriod = config.period as 'daily' | 'weekly' | 'monthly';
    error = '';
  }

  function editRuleConfiguration(rule: NotificationRule) {
    const configuration: Record<string, unknown> = {};
    for (const field of eventConfigFields[rule.event] ?? []) {
      const value = (editRuleConfig[field.key] ?? '').trim();
      if (!value) continue;
      configuration[field.key] = /^(threshold|recovery_threshold)$/.test(
        field.key
      )
        ? value
        : /^\d+$/.test(value)
          ? Number(value)
          : value;
    }
    if (rule.event === 'route.latency') configuration.metric = editRuleMetric;
    if (rule.event === 'report.spend') configuration.period = editRulePeriod;
    return configuration;
  }

  async function saveRule(rule: NotificationRule) {
    if (
      !canManage ||
      editRuleBusy ||
      !editRuleName.trim() ||
      !editRuleDestinationId
    )
      return;
    editRuleBusy = true;
    error = notice = '';
    try {
      await updateNotificationRule(rule, {
        name: editRuleName.trim(),
        destination_id: editRuleDestinationId,
        configuration: editRuleConfiguration(rule)
      });
      editRuleId = '';
      notice = 'Rule updated.';
      await queryClient.invalidateQueries({
        queryKey: notificationKeys.root
      });
    } catch (cause) {
      error = errorMessage(cause, 'The rule could not be updated.');
    } finally {
      editRuleBusy = false;
    }
  }

  async function toggleRule(rule: NotificationRule) {
    if (!canManage) return;
    error = notice = '';
    try {
      await updateNotificationRule(rule, { enabled: !rule.enabled });
      await queryClient.invalidateQueries({
        queryKey: notificationKeys.root
      });
    } catch (cause) {
      error = errorMessage(cause, 'The rule could not be updated.');
    }
  }

  function subjectLabel(rule: NotificationRule) {
    if (rule.event === 'provider.grant.lapsed') return 'Every provider';
    if (rule.subject_id == null) return '—';
    return `${rule.subject_kind === 'api_key' ? 'API key' : 'Budget group'} · ${rule.subject_name ?? rule.subject_id}`;
  }

  function configurationLabel(rule: NotificationRule) {
    const config = (rule.configuration ?? {}) as Record<string, unknown>;
    const parts: string[] = [];
    for (const [key, value] of Object.entries(config)) {
      parts.push(`${key}=${String(value)}`);
    }
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

  function amount(value: string | null, currency: string | null) {
    if (value == null) return '—';
    return currency ? `${value} ${currency}` : value;
  }
</script>

<section
  class="card notifications-panel"
  aria-labelledby="notifications-heading"
>
  <p class="eyebrow">Notifications</p>
  <h2 id="notifications-heading">Notification destinations and rules</h2>
  <p class="section-help">
    {#if services.notificationsActive}Budget thresholds, provider signals,
      runtime health and key expiry or rotation dates send metadata-only
      notifications. Failed deliveries retry with backoff.{:else}Rules and
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
          <label for="dest-type">Type</label><select
            id="dest-type"
            bind:value={destType}
            >{#each destinationTypes as option (option.value)}<option
                value={option.value}>{option.label}</option
              >{/each}</select
          >
        </div>
        <div class="form-field">
          <label for="dest-url">
            {#if isChat(destType)}Channel origin URL{:else if destType === 'pagerduty'}Events
              API endpoint{:else if destType === 'email'}SMTP URL{:else}Webhook
              URL{/if}</label
          ><input
            id="dest-url"
            type="url"
            bind:value={destUrl}
            required
            placeholder={isChat(destType)
              ? 'https://hooks.slack.com'
              : destType === 'pagerduty'
                ? 'https://events.pagerduty.com/v2/enqueue'
                : destType === 'email'
                  ? 'smtps://smtp.example.com:465'
                  : 'https://hooks.example.com/olp'}
          />
        </div>
        <ProjectScopeField
          id="dest-project"
          bind:value={destProjectId}
          unassigned={installationWide}
        />
        {#if isChat(destType)}
          <div class="form-field">
            <label for="dest-webhook-url">Channel webhook URL</label><input
              id="dest-webhook-url"
              bind:value={destWebhookUrl}
              autocomplete="off"
              required
              placeholder="Full tokenized webhook URL, sealed"
            />
            <p class="section-help">
              Stored sealed; never shown again. Must share the origin URL above.
            </p>
          </div>
        {:else if destType === 'pagerduty'}
          <div class="form-field">
            <label for="dest-routing-key">Routing key</label><input
              id="dest-routing-key"
              bind:value={destRoutingKey}
              autocomplete="off"
              required
              placeholder="PagerDuty Events API routing key, sealed"
            />
          </div>
        {:else if destType === 'email'}
          <div class="form-field">
            <label for="dest-email-from">From address</label><input
              id="dest-email-from"
              bind:value={destEmailFrom}
              required
              placeholder="alerts@example.com"
            />
          </div>
          <div class="form-field">
            <label for="dest-email-to">To addresses</label><input
              id="dest-email-to"
              bind:value={destEmailTo}
              required
              placeholder="ops@example.com, dev@example.com"
            />
          </div>
          <div class="form-field">
            <label for="dest-email-prefix">Subject prefix</label><input
              id="dest-email-prefix"
              bind:value={destEmailSubjectPrefix}
              placeholder="Optional [OLP] prefix"
            />
          </div>
          <div class="form-field">
            <label for="dest-email-ca">CA certificate (PEM)</label><textarea
              id="dest-email-ca"
              bind:value={destEmailCa}
              rows="3"
              placeholder="Optional pinned CA bundle"></textarea>
          </div>
          <div class="form-field">
            <label for="dest-email-username">SMTP username</label><input
              id="dest-email-username"
              bind:value={destEmailUsername}
              autocomplete="off"
              placeholder="Optional"
            />
          </div>
          <div class="form-field">
            <label for="dest-email-password">SMTP password</label><input
              id="dest-email-password"
              type="password"
              bind:value={destEmailPassword}
              autocomplete="new-password"
              placeholder="Optional, sealed"
            />
          </div>
        {:else}
          <div class="form-field">
            <label for="dest-secret">Signing secret</label><input
              id="dest-secret"
              bind:value={destSecret}
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
          disabled={destBusy || !destName.trim() || !destUrl.trim()}
          >{destBusy ? 'Creating…' : 'Create destination'}</button
        >
      </div>
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
                    bind:value={editDestName}
                    required
                  /></td
                ><td>{destination.type ?? 'webhook'}</td><td
                  ><input
                    aria-label="Destination URL"
                    type="url"
                    bind:value={editDestUrl}
                    required
                  /></td
                ><td>{destination.project_name ?? 'Installation-wide'}</td><td
                  ><input
                    aria-label="Replacement secret"
                    bind:value={editDestSecret}
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
                      />Clear credential</label
                    >{/if}
                  {#if (destination.type ?? 'webhook') === 'email'}<div
                      class="form-grid"
                    >
                      <input
                        aria-label="From address"
                        bind:value={editEmailFrom}
                        placeholder="From"
                      /><input
                        aria-label="To addresses"
                        bind:value={editEmailTo}
                        placeholder="To, comma-separated"
                      /><input
                        aria-label="Subject prefix"
                        bind:value={editEmailPrefix}
                        placeholder="Subject prefix"
                      /><input
                        aria-label="CA certificate"
                        bind:value={editEmailCa}
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
                ><td>{destination.secret_configured ? 'Configured' : '—'}</td
                ><td>{destination.enabled ? 'Yes' : 'No'}</td>{#if canManage}<td
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

  <h3>Rules</h3>
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
        <div class="form-field">
          <label for="rule-event">Event</label><select
            id="rule-event"
            bind:value={ruleEvent}
            >{#each Object.entries(eventLabels) as [value, label] (value)}<option
                {value}
                disabled={!installationWide &&
                  !projectScopedEvents.has(value as NotificationEvent)}
                >{label}</option
              >{/each}</select
          >
        </div>
        {#if ruleProjectScoped}
          <ProjectScopeField
            id="rule-project"
            bind:value={ruleProjectId}
            unassigned={installationWide}
          />
        {/if}
        {#if watchesBudget || watchesKey}
          <div class="form-field">
            <label for="rule-subject-kind">Subject</label><select
              id="rule-subject-kind"
              disabled={watchesKey}
              bind:value={ruleSubjectKind}
              ><option value="api_key">API key</option><option
                value="budget_group">Budget group</option
              ></select
            >
          </div>
          <div class="form-field">
            <label for="rule-subject">
              {ruleSubjectKind === 'api_key'
                ? 'API key'
                : 'Budget group'}</label
            ><select id="rule-subject" bind:value={ruleSubjectId} required
              ><option value="" disabled>Choose a subject</option
              >{#each subjectOptions as subject (subject.id)}<option
                  value={subject.id}>{subject.name}</option
                >{/each}</select
            >
          </div>
          {#if watchesBudget}
            <div class="form-field">
              <label for="rule-window">Window</label><select
                id="rule-window"
                bind:value={ruleWindow}
                ><option value="day">Budget day</option><option value="week"
                  >Budget week</option
                ><option value="month">Budget month</option></select
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
          {/if}
        {/if}
        {#each eventConfigFields[ruleEvent] ?? [] as field (field.key)}
          <div class="form-field">
            <label for="rule-config-{field.key}">{field.label}</label><input
              id="rule-config-{field.key}"
              inputmode={field.inputmode}
              value={ruleConfig[field.key] ?? ''}
              oninput={(e) =>
                (ruleConfig = {
                  ...ruleConfig,
                  [field.key]: (e.target as HTMLInputElement).value
                })}
              placeholder="Default"
            />
          </div>
        {/each}
        {#if ruleEvent === 'route.latency'}
          <div class="form-field">
            <label for="rule-metric">Metric</label><select
              id="rule-metric"
              bind:value={ruleMetric}
              ><option value="ttft">Time to first token</option><option
                value="latency">Total latency</option
              ></select
            >
          </div>
        {/if}
        {#if ruleEvent === 'report.spend'}
          <div class="form-field">
            <label for="rule-period">Period</label><select
              id="rule-period"
              bind:value={rulePeriod}
              ><option value="daily">Daily</option><option value="weekly"
                >Weekly</option
              ><option value="monthly">Monthly</option></select
            >
          </div>
        {/if}
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
            ((watchesBudget || watchesKey) && !ruleSubjectId) ||
            !ruleDestinationId}>{ruleBusy ? 'Creating…' : 'Create rule'}</button
        >
      </div>
      <p class="section-help">
        {#if watchesBudget}A delivery is sent when a subject's accrued spend
          crosses the threshold within the window.{:else if watchesKey}Sent once
          when the key expires or reaches its rotation date within the lead
          time.{:else if genericRule}Metadata-only trigger and recovery
          deliveries; cooldown repeats share the incident key.{:else}A grant
          lapses when its provider plugin can no longer refresh it.{/if}
      </p>
    </form>
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
                    required
                  /><br /><small
                    >{rule.project_name ?? 'Installation-wide'}</small
                  ></td
                ><td>{eventLabels[rule.event]}</td><td>{subjectLabel(rule)}</td
                ><td>{windowLabel(rule.window_kind)}</td><td
                  ><div class="form-grid">
                    {#each eventConfigFields[rule.event] ?? [] as field (field.key)}
                      <input
                        aria-label={field.label}
                        inputmode={field.inputmode}
                        value={editRuleConfig[field.key] ?? ''}
                        oninput={(e) =>
                          (editRuleConfig = {
                            ...editRuleConfig,
                            [field.key]: (e.target as HTMLInputElement).value
                          })}
                        placeholder={`${field.key} (default)`}
                      />
                    {/each}
                    {#if rule.event === 'route.latency'}<select
                        aria-label="Metric"
                        bind:value={editRuleMetric}
                        ><option value="ttft">Time to first token</option
                        ><option value="latency">Total latency</option></select
                      >{/if}
                    {#if rule.event === 'report.spend'}<select
                        aria-label="Period"
                        bind:value={editRulePeriod}
                        ><option value="daily">Daily</option><option
                          value="weekly">Weekly</option
                        ><option value="monthly">Monthly</option></select
                      >{/if}
                  </div></td
                ><td
                  ><select
                    aria-label="Destination"
                    bind:value={editRuleDestinationId}
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
                    onclick={() => (editRuleId = '')}>Cancel</button
                  ></td
                ></tr
              >
            {:else}
              <tr
                ><td
                  ><strong>{rule.name}</strong><br /><small
                    >{rule.project_name ?? 'Installation-wide'}</small
                  ></td
                ><td>{eventLabels[rule.event]}</td><td>{subjectLabel(rule)}</td
                ><td>{windowLabel(rule.window_kind)}</td><td
                  ><span class="mono">{configurationLabel(rule)}</span></td
                ><td>{rule.destination_name}</td><td
                  >{rule.enabled ? 'Yes' : 'No'}</td
                >{#if canManage}<td
                    ><button
                      class="text-button"
                      type="button"
                      onclick={() => startRuleEdit(rule)}>Edit</button
                    ><button
                      class="text-button"
                      type="button"
                      onclick={() => toggleRule(rule)}
                      >{rule.enabled ? 'Disable' : 'Enable'}</button
                    ></td
                  >{/if}</tr
              >
            {/if}
          {/each}
        </tbody>
      </table>
    </div>
  {/if}

  <h3>Deliveries</h3>
  {#if deliveries.isPending}<span class="inline-status" role="status"
      >Loading deliveries…</span
    >
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
            ><th scope="col">Rule</th><th scope="col">Event</th><th scope="col"
              >Window</th
            ><th scope="col">Threshold</th><th scope="col">Accrued</th><th
              scope="col">Limit</th
            ><th scope="col">Status</th><th scope="col">Attempts</th><th
              scope="col">Last error</th
            ><th scope="col">Last attempt</th></tr
          ></thead
        >
        <tbody>
          {#each deliveries.data ?? [] as delivery (delivery.id)}
            <tr
              ><td>{delivery.rule_name}</td><td
                >{eventLabels[
                  delivery.event
                ]}{#if delivery.event === 'key.expiring'}<small
                    >{delivery.api_key_name} · {delivery.reason}
                    {delivery.due_at
                      ? new Date(delivery.due_at).toLocaleString()
                      : ''}</small
                  >{/if}
                {#if delivery.provider_name}<br /><small
                    >{delivery.provider_name} · credential v{delivery.credential_version}</small
                  >{/if}</td
              ><td><span class="mono">{delivery.window_id ?? '—'}</span></td><td
                >{delivery.threshold_percent == null
                  ? '—'
                  : `${delivery.threshold_percent}%`}</td
              ><td
                ><span class="mono"
                  >{amount(delivery.accrued, delivery.currency)}</span
                ></td
              ><td
                ><span class="mono"
                  >{amount(delivery.limit, delivery.currency)}</span
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
  .inline-check {
    display: flex;
    align-items: center;
    gap: 0.35rem;
    font-size: var(--text-caption);
    color: var(--foreground-muted);
  }
</style>
