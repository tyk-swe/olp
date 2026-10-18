<script lang="ts">
  import { untrack } from 'svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { holds } from '$lib/features/access/session/authorization';
  import ProjectScopeField from '$lib/features/access/projects/ProjectScopeField.svelte';
  import { listProjectMemberships } from '$lib/features/access/projects/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
  import { formatCost, formatDate } from '$lib/format';
  import {
    createCapturePolicy,
    createExportSink,
    deleteCapturePolicy,
    deleteExportSink,
    getCaptureConfiguration,
    listCapturePolicies,
    listCaptureSinkOptions,
    listExportSinks,
    listExportSinkFocusDatasets,
    listExportSinkGaps,
    updateCaptureConfiguration,
    updateCapturePolicy,
    updateExportSink,
    type CapturePolicy,
    type ExportSink
  } from '$lib/features/observability/api';
  import {
    buildSinkSettings,
    validateSinkSettings
  } from '$lib/features/observability/sinkSettings';
  import {
    focusDatasetStatus,
    lagSeconds,
    sinkSettingsSummary,
    streamState,
    streamStatuses
  } from '$lib/features/observability/presentation';
  import { observabilityKeys } from '$lib/features/observability/observabilityKeys';

  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '$lib/forms/mutationIntent.svelte';

  const sinkIntent = useMutationIntent();
  const policyIntent = useMutationIntent();

  const queryClient = useQueryClient();
  const access = useRole();
  const globalScope = $derived(access.user?.access_scope === 'global');
  const ownerOnly = $derived(access.user?.role === 'owner' && globalScope);
  const canManageInstallation = $derived(
    globalScope && holds(access.user, 'settings')
  );
  const canManageProjects = $derived(access.can('api_keys.manage'));
  const memberships = createQuery(() => ({
    queryKey: projectKeys.memberships,
    queryFn: ({ signal }) => listProjectMemberships(signal)
  }));
  const writableProjects = $derived(
    (memberships.data ?? [])
      .filter((m) => m.role === 'manager')
      .map((m) => m.id)
  );

  function canMutate(resourceProject: string | null | undefined) {
    if (resourceProject == null) return canManageInstallation;
    return canManageProjects && writableProjects.includes(resourceProject);
  }

  const sinks = createQuery(() => ({
    queryKey: observabilityKeys.sinks(),
    queryFn: ({ signal }) => listExportSinks(signal)
  }));
  const captureConfig = createQuery(() => ({
    queryKey: observabilityKeys.capture(),
    queryFn: ({ signal }) => getCaptureConfiguration(signal)
  }));
  const policies = createQuery(() => ({
    queryKey: observabilityKeys.capturePolicies(),
    queryFn: ({ signal }) => listCapturePolicies(signal)
  }));
  const sinkOptions = createQuery(() => ({
    queryKey: observabilityKeys.captureSinks(),
    queryFn: ({ signal }) => listCaptureSinkOptions(signal)
  }));

  let error = $state('');
  let notice = $state('');

  const typeFormats: Record<string, { value: string; label: string }[]> = {
    https: [
      { value: 'json', label: 'JSON' },
      { value: 'cloudevents', label: 'CloudEvents' }
    ],
    otlp_logs: [{ value: 'otlp', label: 'OTLP logs' }],
    s3: [
      { value: 'jsonl', label: 'JSON Lines' },
      { value: 'csv_gzip', label: 'FOCUS 1.4 (gzip CSV)' }
    ],
    gcs: [
      { value: 'jsonl', label: 'JSON Lines' },
      { value: 'csv_gzip', label: 'FOCUS 1.4 (gzip CSV)' }
    ],
    azure_blob: [
      { value: 'jsonl', label: 'JSON Lines' },
      { value: 'csv_gzip', label: 'FOCUS 1.4 (gzip CSV)' }
    ],
    stripe_meter: [{ value: 'stripe', label: 'Stripe meter events' }]
  };
  const typeLabels: Record<string, string> = {
    https: 'HTTPS',
    otlp_logs: 'OTLP logs',
    s3: 'S3',
    gcs: 'GCS',
    azure_blob: 'Azure Blob',
    stripe_meter: 'Stripe meter'
  };
  const streamOptions = [
    { value: 'requests', label: 'Requests' },
    { value: 'attempts', label: 'Attempts' },
    { value: 'usage_rollups', label: 'Usage rollups' },
    { value: 'guardrail_decisions', label: 'Guardrail decisions' },
    { value: 'audit', label: 'Audit' },
    { value: 'metering', label: 'Metering' },
    { value: 'focus', label: 'FOCUS' }
  ];
  const stripeMeterValues = [
    { value: 'charge', label: 'Charge' },
    { value: 'cost', label: 'Cost' },
    { value: 'input_tokens', label: 'Input tokens' },
    { value: 'output_tokens', label: 'Output tokens' },
    { value: 'total_tokens', label: 'Total tokens' },
    { value: 'requests', label: 'Requests' }
  ];
  const isoDay = (offset: number) =>
    new Date(Date.now() - offset * 24 * 60 * 60 * 1000)
      .toISOString()
      .slice(0, 10);
  const focusStartMin = isoDay(89);
  const focusStartMax = isoDay(0);

  let editingSink = $state<ExportSink | null>(null);
  let sinkName = $state('');
  let sinkType = $state<ExportSink['type']>('https');
  let sinkDestination = $state('');
  let sinkProjectId = $state('');
  let sinkFilterProject = $state('');
  let sinkStreams = $state<string[]>(['requests']);
  let sinkFormat = $state<string>('json');
  let sinkFilterRoute = $state('');
  let sinkFilterOutcome = $state('');
  let sinkCredential = $state('');
  let sinkClearCredential = $state(false);
  let sinkStripeKey = $state('');
  let sinkFocusCostBasis = $state<'cost' | 'charge'>('cost');
  let sinkFocusStartDate = $state('');
  let sinkCeSource = $state('');
  let sinkStripeEvent = $state('olp.charge');
  let sinkStripeValue = $state('charge');
  let sinkCustomers = $state<{ project: string; customer: string }[]>([
    { project: '', customer: '' }
  ]);
  let sinkStripeCustomerKey = $state('');
  let sinkStripeValueKey = $state('');
  let sinkStripeAdvanced = $state(false);
  let sinkBusy = $state(false);

  // The server's pairing matrix: cloudevents and stripe carry exactly
  // metering, csv_gzip carries exactly FOCUS, and FOCUS is not selectable on
  // any other format.
  const streamLock = $derived.by((): string[] | null => {
    if (sinkFormat === 'cloudevents' || sinkFormat === 'stripe')
      return ['metering'];
    if (sinkFormat === 'csv_gzip') return ['focus'];
    return null;
  });
  const focusVisible = $derived(sinkFormat === 'csv_gzip');
  const outcomeVisible = $derived(sinkFormat !== 'csv_gzip');

  $effect(() => {
    const formats = typeFormats[sinkType] ?? typeFormats.https;
    if (!formats.some((f) => f.value === sinkFormat))
      sinkFormat = formats[0].value;
  });

  $effect(() => {
    // Equal-content writes stay untouched so this effect settles.
    if (
      streamLock &&
      (sinkStreams.length !== streamLock.length ||
        sinkStreams.some((stream, index) => stream !== streamLock[index]))
    )
      sinkStreams = [...streamLock];
    if (!focusVisible && sinkStreams.includes('focus'))
      sinkStreams = sinkStreams.filter((stream) => stream !== 'focus');
  });

  $effect(() => {
    // Stripe destinations default to the meter API but stay editable.
    const stripe = !editingSink && sinkType === 'stripe_meter';
    untrack(() => {
      if (stripe && !sinkDestination)
        sinkDestination = 'https://api.stripe.com';
    });
  });

  function toggleStream(stream: string) {
    if (streamLock) return;
    sinkStreams = sinkStreams.includes(stream)
      ? sinkStreams.filter((value) => value !== stream)
      : [...sinkStreams, stream];
  }

  function sinkFilterValue() {
    const filter: Record<string, unknown> = {};
    if (sinkFilterProject) filter.project = sinkFilterProject;
    if (sinkFilterRoute.trim()) filter.route = sinkFilterRoute.trim();
    if (sinkFilterOutcome && outcomeVisible) filter.outcome = sinkFilterOutcome;
    return Object.keys(filter).length ? filter : null;
  }

  function startSinkEdit(sink: ExportSink) {
    if (sinkBusy) return;
    sinkIntent.reset();
    editingSink = sink;
    sinkName = sink.name;
    sinkType = sink.type;
    sinkDestination = sink.destination;
    sinkProjectId = sink.project_id ?? '';
    const filter = (sink.filter ?? {}) as Record<string, string>;
    sinkFilterProject = filter.project ?? '';
    sinkFilterRoute = filter.route ?? '';
    sinkFilterOutcome = filter.outcome ?? '';
    sinkStreams = [...sink.streams];
    sinkFormat = sink.format;
    sinkCredential = '';
    sinkClearCredential = false;
    sinkStripeKey = '';
    const settings = (sink.settings ?? {}) as Record<string, unknown>;
    sinkFocusCostBasis = settings.cost_basis === 'charge' ? 'charge' : 'cost';
    sinkFocusStartDate =
      typeof settings.start_date === 'string' ? settings.start_date : '';
    sinkCeSource = typeof settings.source === 'string' ? settings.source : '';
    sinkStripeEvent =
      typeof settings.event_name === 'string'
        ? settings.event_name
        : 'olp.charge';
    sinkStripeValue =
      typeof settings.value === 'string' ? settings.value : 'charge';
    sinkCustomers = Object.entries(
      (settings.customers ?? {}) as Record<string, string>
    ).map(([project, customer]) => ({ project, customer }));
    if (!sinkCustomers.length) sinkCustomers = [{ project: '', customer: '' }];
    sinkStripeCustomerKey =
      typeof settings.customer_key === 'string' ? settings.customer_key : '';
    sinkStripeValueKey =
      typeof settings.value_key === 'string' ? settings.value_key : '';
    sinkStripeAdvanced = !!(sinkStripeCustomerKey || sinkStripeValueKey);
    error = '';
  }

  function resetSinkForm() {
    sinkIntent.reset();
    editingSink = null;
    sinkName = '';
    sinkDestination = '';
    sinkFilterRoute = '';
    sinkFilterOutcome = '';
    sinkFilterProject = '';
    sinkCredential = '';
    sinkClearCredential = false;
    sinkStripeKey = '';
    sinkFocusCostBasis = 'cost';
    sinkFocusStartDate = '';
    sinkCeSource = '';
    sinkStripeEvent = 'olp.charge';
    sinkStripeValue = 'charge';
    sinkCustomers = [{ project: '', customer: '' }];
    sinkStripeCustomerKey = sinkStripeValueKey = '';
    sinkStripeAdvanced = false;
  }

  function cancelSinkEdit() {
    if (!sinkBusy) resetSinkForm();
  }

  async function submitSink(event: SubmitEvent) {
    event.preventDefault();
    const editing = editingSink;
    const allowed = editing
      ? canMutate(editing.project_id)
      : canMutate(sinkProjectId || null);
    if (!allowed || sinkBusy || !sinkName.trim() || !sinkDestination.trim())
      return;
    const isCurrent = captureOwner();
    sinkBusy = true;
    error = notice = '';
    let committed = false;
    try {
      const draft = {
        format: sinkFormat,
        focusCostBasis: sinkFocusCostBasis,
        focusStartDate: sinkFocusStartDate,
        ceSource: sinkCeSource,
        stripeEvent: sinkStripeEvent,
        stripeValue: sinkStripeValue,
        customers: sinkCustomers,
        stripeCustomerKey: sinkStripeCustomerKey,
        stripeValueKey: sinkStripeValueKey
      };
      const settingsError = validateSinkSettings(draft, {
        editing: !!editing,
        stripeKey: sinkStripeKey
      });
      if (settingsError) {
        error = settingsError;
        return;
      }
      const settings = buildSinkSettings(draft);
      let credential: unknown;
      if (sinkType === 'stripe_meter') {
        // A Stripe sink carries exactly one credential shape.
        if (sinkStripeKey.trim())
          credential = { api_key: sinkStripeKey.trim() };
      } else if (sinkCredential.trim()) {
        try {
          credential = JSON.parse(sinkCredential);
        } catch {
          error = 'Credential JSON is not valid JSON.';
          return;
        }
      }
      if (editing) {
        await updateExportSink(editing, {
          name: sinkName.trim(),
          destination: sinkDestination.trim(),
          filter: sinkFilterValue(),
          ...(settings ? { settings } : {}),
          ...(sinkClearCredential
            ? { credential: null }
            : credential !== undefined
              ? { credential: credential as Record<string, unknown> }
              : {})
        });
        committed = true;
        if (!isCurrent()) return;
        notice = 'Sink updated.';
      } else {
        if (!sinkStreams.length) return;
        const submission = sinkIntent.prepare<
          Parameters<typeof createExportSink>[0]
        >({
          name: sinkName.trim(),
          type: sinkType,
          destination: sinkDestination.trim(),
          project_id: sinkProjectId || null,
          streams: sinkStreams as ExportSink['streams'],
          format: sinkFormat as ExportSink['format'],
          filter: sinkFilterValue(),
          settings,
          ...(credential !== undefined
            ? { credential: credential as Record<string, unknown> }
            : {})
        });
        await createExportSink(submission.input, submission.requestId);
        committed = true;
        if (!isCurrent()) return;
        sinkIntent.reset();
        notice = 'Sink created.';
      }
      resetSinkForm();
      await queryClient.invalidateQueries(
        {
          queryKey: observabilityKeys.root
        },
        { throwOnError: true }
      );
    } catch (cause) {
      if (!isCurrent()) return;
      error = committed
        ? `${notice} Refresh failed. ${errorMessage(cause)}`
        : errorMessage(
            cause,
            editing
              ? 'The sink could not be updated.'
              : 'The sink could not be created.'
          );
    } finally {
      if (isCurrent()) sinkBusy = false;
    }
  }

  async function toggleSink(sink: ExportSink) {
    if (!canMutate(sink.project_id)) return;
    const isCurrent = captureOwner();
    error = notice = '';
    try {
      await updateExportSink(sink, { enabled: !sink.enabled });
      if (!isCurrent()) return;
      await queryClient.invalidateQueries({ queryKey: observabilityKeys.root });
    } catch (cause) {
      if (!isCurrent()) return;
      error = errorMessage(cause, 'The sink could not be updated.');
    }
  }

  async function removeSink(sink: ExportSink) {
    if (!canMutate(sink.project_id)) return;
    const isCurrent = captureOwner();
    error = notice = '';
    try {
      await deleteExportSink(sink);
      if (!isCurrent()) return;
      notice = 'Sink deleted.';
      await queryClient.invalidateQueries({ queryKey: observabilityKeys.root });
    } catch (cause) {
      if (!isCurrent()) return;
      error = errorMessage(cause, 'The sink could not be deleted.');
    }
  }

  let editingPolicy = $state<CapturePolicy | null>(null);
  let policyProjectId = $state('');
  let policyRoute = $state('');
  let policySinkId = $state('');
  let policyRatio = $state('1');
  let policyInclude = $state<string[]>(['input', 'output']);
  let policyKeyIds = $state('');
  let policyEndUsers = $state('');
  let policyMaxBytes = $state('65536');
  let policyEnabled = $state(true);
  let policyBusy = $state(false);

  function toggleInclude(part: string) {
    policyInclude = policyInclude.includes(part)
      ? policyInclude.filter((value) => value !== part)
      : [...policyInclude, part];
  }

  function startPolicyEdit(policy: CapturePolicy) {
    if (policyBusy) return;
    policyIntent.reset();
    editingPolicy = policy;
    policyProjectId = policy.project_id ?? '';
    policyRoute = policy.route_slug ?? '';
    policySinkId = policy.sink;
    policyRatio = policy.sample_ratio;
    policyInclude = [...policy.include];
    policyKeyIds = policy.key_ids.join(', ');
    policyEndUsers = policy.end_user_digests.join(', ');
    policyMaxBytes = String(policy.max_bytes);
    policyEnabled = policy.enabled;
    error = '';
  }

  function resetPolicyForm() {
    policyIntent.reset();
    editingPolicy = null;
    policyRoute = policyKeyIds = policyEndUsers = '';
    policySinkId = '';
    policyRatio = '1';
    policyEnabled = true;
  }

  function cancelPolicyEdit() {
    if (!policyBusy) resetPolicyForm();
  }

  const captureOwner = usePrincipalLifetime(() => {
    resetSinkForm();
    resetPolicyForm();
    sinkBusy = policyBusy = false;
    error = notice = '';
  });

  async function toggleMaster() {
    if (!ownerOnly || !captureConfig.data) return;
    const isCurrent = captureOwner();
    error = notice = '';
    try {
      await updateCaptureConfiguration(captureConfig.data, {
        enabled: !captureConfig.data.enabled
      });
      if (!isCurrent()) return;
      await queryClient.invalidateQueries({
        queryKey: observabilityKeys.capture()
      });
    } catch (cause) {
      if (!isCurrent()) return;
      error = errorMessage(cause, 'Capture could not be changed.');
    }
  }

  async function submitPolicy(event: SubmitEvent) {
    event.preventDefault();
    const editing = editingPolicy;
    const allowed = canMutate(
      editing ? editing.project_id : policyProjectId || null
    );
    if (
      !allowed ||
      policyBusy ||
      !policySinkId ||
      !policyRatio.trim() ||
      !policyInclude.length
    )
      return;
    const maxBytes = Number(policyMaxBytes);
    if (!Number.isInteger(maxBytes) || maxBytes < 1) {
      error = 'Max bytes must be a positive integer.';
      return;
    }
    const isCurrent = captureOwner();
    policyBusy = true;
    error = notice = '';
    let committed = false;
    const fields = {
      sink: policySinkId,
      sample_ratio: policyRatio.trim(),
      include: policyInclude as CapturePolicy['include'],
      key_ids: policyKeyIds
        .split(',')
        .map((v) => v.trim())
        .filter(Boolean),
      end_user_digests: policyEndUsers
        .split(',')
        .map((v) => v.trim())
        .filter(Boolean),
      max_bytes: maxBytes,
      enabled: policyEnabled
    };
    try {
      if (editing) {
        await updateCapturePolicy(editing, fields);
        committed = true;
        if (!isCurrent()) return;
        notice = 'Capture policy updated.';
      } else {
        const submission = policyIntent.prepare<
          Parameters<typeof createCapturePolicy>[0]
        >({
          project_id: policyProjectId || null,
          route_slug: policyRoute.trim() || null,
          ...fields
        });
        await createCapturePolicy(submission.input, submission.requestId);
        committed = true;
        if (!isCurrent()) return;
        policyIntent.reset();
        notice = 'Capture policy created.';
      }
      resetPolicyForm();
      await queryClient.invalidateQueries(
        {
          queryKey: observabilityKeys.capturePolicies()
        },
        { throwOnError: true }
      );
    } catch (cause) {
      if (!isCurrent()) return;
      error = committed
        ? `${notice} Refresh failed. ${errorMessage(cause)}`
        : errorMessage(
            cause,
            editing
              ? 'The capture policy could not be updated.'
              : 'The capture policy could not be created.'
          );
    } finally {
      if (isCurrent()) policyBusy = false;
    }
  }

  async function togglePolicy(policy: CapturePolicy) {
    if (!canMutate(policy.project_id)) return;
    const isCurrent = captureOwner();
    error = notice = '';
    try {
      await updateCapturePolicy(policy, { enabled: !policy.enabled });
      if (!isCurrent()) return;
      await queryClient.invalidateQueries({
        queryKey: observabilityKeys.capturePolicies()
      });
    } catch (cause) {
      if (!isCurrent()) return;
      error = errorMessage(cause, 'The capture policy could not be updated.');
    }
  }

  async function removePolicy(policy: CapturePolicy) {
    if (!canMutate(policy.project_id)) return;
    const isCurrent = captureOwner();
    error = notice = '';
    try {
      await deleteCapturePolicy(policy);
      if (!isCurrent()) return;
      notice = 'Capture policy deleted.';
      await queryClient.invalidateQueries({
        queryKey: observabilityKeys.capturePolicies()
      });
    } catch (cause) {
      if (!isCurrent()) return;
      error = errorMessage(cause, 'The capture policy could not be deleted.');
    }
  }

  let gapSinkId = $state('');
  const gapQuery = createQuery(() => ({
    queryKey: [...observabilityKeys.sinks(), 'gaps', gapSinkId],
    queryFn: ({ signal }) => listExportSinkGaps(gapSinkId, signal),
    enabled: gapSinkId !== ''
  }));

  function toggleGaps(sink: ExportSink) {
    gapSinkId = gapSinkId === sink.export_sink_id ? '' : sink.export_sink_id;
  }

  let datasetSinkId = $state('');
  const focusDatasets = createQuery(() => ({
    queryKey: observabilityKeys.focusDatasets(datasetSinkId),
    queryFn: ({ signal }) => listExportSinkFocusDatasets(datasetSinkId, signal),
    enabled: datasetSinkId !== ''
  }));

  function toggleDatasets(sink: ExportSink) {
    datasetSinkId =
      datasetSinkId === sink.export_sink_id ? '' : sink.export_sink_id;
  }

  function sinkOptionLabel(id: string) {
    const option = (sinkOptions.data ?? []).find(
      (s) => s.export_sink_id === id
    );
    return option ? `${option.name} (${option.type})` : id;
  }
</script>

<section
  class="card observability-panel"
  aria-labelledby="observability-heading"
>
  <p class="eyebrow">Observability</p>
  <h2 id="observability-heading">Export sinks and payload capture</h2>
  <p class="section-help">
    Durable metadata and usage export sinks deliver to HTTPS, OTLP logs or
    object storage. Bounded payload capture is opt-in and unredacted.
  </p>

  {#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
  {#if notice}<p class="section-help" role="status">{notice}</p>{/if}

  <h3>Export sinks</h3>
  {#if canManageInstallation || canManageProjects}
    <form class="create-form" onsubmit={submitSink}>
      <fieldset class="submission-fields" disabled={sinkBusy}>
        <div class="form-grid">
          <div class="form-field">
            <label for="sink-name">Name</label><input
              id="sink-name"
              bind:value={sinkName}
              required
            />
          </div>
          <div class="form-field">
            <label for="sink-type">Type</label><select
              id="sink-type"
              bind:value={sinkType}
              disabled={!!editingSink}
              >{#each Object.keys(typeFormats) as type (type)}<option
                  value={type}>{typeLabels[type] ?? type}</option
                >{/each}</select
            >
          </div>
          <div class="form-field">
            <label for="sink-destination">Destination</label><input
              id="sink-destination"
              bind:value={sinkDestination}
              required
              placeholder="https://collector.example.com/olp or bucket URL"
            />
          </div>
          <ProjectScopeField
            id="sink-project"
            bind:value={sinkProjectId}
            unassigned={canManageInstallation}
            disabled={!!editingSink}
          />
          <div class="form-field">
            <label for="sink-format">Format</label><select
              id="sink-format"
              bind:value={sinkFormat}
              disabled={!!editingSink}
              >{#each typeFormats[sinkType] ?? [] as option (option.value)}<option
                  value={option.value}>{option.label}</option
                >{/each}</select
            >
          </div>
          <fieldset class="form-field">
            <legend>Streams</legend>
            {#each streamOptions as stream (stream.value)}
              {#if stream.value !== 'focus' || focusVisible}
                <label class="inline-check"
                  ><input
                    type="checkbox"
                    checked={sinkStreams.includes(stream.value)}
                    disabled={!!editingSink || streamLock !== null}
                    onchange={() => toggleStream(stream.value)}
                  />{stream.label}</label
                >
              {/if}
            {/each}
            {#if streamLock}<small
                >This format carries {streamLock[0]} only.</small
              >{/if}
          </fieldset>
          <div class="form-field">
            <label for="sink-filter-project">Filter project</label><input
              id="sink-filter-project"
              class="mono"
              bind:value={sinkFilterProject}
              placeholder="Optional project UUID"
            />
          </div>
          <div class="form-field">
            <label for="sink-filter-route">Route filter</label><input
              id="sink-filter-route"
              bind:value={sinkFilterRoute}
              placeholder="Optional route slug"
            />
          </div>
          {#if outcomeVisible}<div class="form-field">
              <label for="sink-filter-outcome">Outcome filter</label><select
                id="sink-filter-outcome"
                bind:value={sinkFilterOutcome}
                ><option value="">All outcomes</option><option value="success"
                  >Success</option
                ><option value="failure">Failure</option></select
              >
            </div>{/if}
          {#if sinkType === 'stripe_meter'}
            <div class="form-field">
              <label for="sink-stripe-key">Restricted API key</label><input
                id="sink-stripe-key"
                type="password"
                autocomplete="off"
                bind:value={sinkStripeKey}
                placeholder={editingSink
                  ? 'Blank keeps the stored key'
                  : 'rk_…'}
              />
            </div>
          {:else}<div class="form-field">
              <label for="sink-credential">Credential JSON</label><textarea
                id="sink-credential"
                bind:value={sinkCredential}
                rows="2"
                placeholder={editingSink
                  ? 'Optional sealed replacement — blank keeps current'
                  : 'Optional sealed credential object'}></textarea>
              {#if editingSink}<label class="inline-check"
                  ><input
                    type="checkbox"
                    aria-label="Clear stored credential"
                    bind:checked={sinkClearCredential}
                  />Clear stored credential</label
                >{/if}
            </div>{/if}
          {#if sinkFormat === 'csv_gzip'}
            <div class="form-field">
              <label for="sink-cost-basis">FOCUS cost basis</label><select
                id="sink-cost-basis"
                bind:value={sinkFocusCostBasis}
                disabled={!!editingSink}
                ><option value="cost">Provider cost</option><option
                  value="charge">Charge</option
                ></select
              >{#if editingSink}<small
                  >Cost basis cannot change after creation.</small
                >{/if}
            </div>
            <div class="form-field">
              <label for="sink-focus-start">FOCUS start date</label><input
                id="sink-focus-start"
                type="date"
                bind:value={sinkFocusStartDate}
                min={focusStartMin}
                max={focusStartMax}
              /><small>Optional — earlier than 90 days ago is rejected.</small>
            </div>
          {:else if sinkFormat === 'cloudevents'}
            <div class="form-field">
              <label for="sink-ce-source">CloudEvents source</label><input
                id="sink-ce-source"
                bind:value={sinkCeSource}
                placeholder="https://example.com/olp"
              />
            </div>
          {:else if sinkFormat === 'stripe'}
            <div class="form-field">
              <label for="sink-stripe-event">Stripe event name</label><input
                id="sink-stripe-event"
                bind:value={sinkStripeEvent}
                required
                placeholder="olp.charge"
              />
            </div>
            <div class="form-field">
              <label for="sink-stripe-value">Meter value</label><select
                id="sink-stripe-value"
                bind:value={sinkStripeValue}
                >{#each stripeMeterValues as option (option.value)}<option
                    value={option.value}>{option.label}</option
                  >{/each}</select
              >
            </div>
            <fieldset class="form-field">
              <legend>Stripe customers</legend>
              {#each sinkCustomers as row, i (i)}
                <div class="customer-row">
                  <select aria-label="Project" bind:value={row.project}
                    >{#each memberships.data ?? [] as membership (membership.id)}<option
                        value={membership.id}
                        >{membership.name ?? membership.id}</option
                      >{/each}</select
                  ><input
                    aria-label="Stripe customer"
                    class="mono"
                    bind:value={row.customer}
                    placeholder="cus_…"
                  /><button
                    class="text-button"
                    type="button"
                    aria-label="Remove mapping"
                    disabled={sinkCustomers.length <= 1}
                    onclick={() =>
                      (sinkCustomers = sinkCustomers.filter(
                        (_, index) => index !== i
                      ))}>&times;</button
                  >
                </div>
              {/each}
              <button
                class="text-button"
                type="button"
                onclick={() =>
                  (sinkCustomers = [
                    ...sinkCustomers,
                    { project: '', customer: '' }
                  ])}>Add mapping</button
              >
            </fieldset>
            <button
              class="text-button"
              type="button"
              aria-expanded={sinkStripeAdvanced}
              onclick={() => (sinkStripeAdvanced = !sinkStripeAdvanced)}
              >Advanced</button
            >
            {#if sinkStripeAdvanced}
              <div class="form-field">
                <label for="sink-stripe-customer-key">Customer key</label><input
                  id="sink-stripe-customer-key"
                  class="mono"
                  bind:value={sinkStripeCustomerKey}
                  placeholder="olp_project"
                />
              </div>
              <div class="form-field">
                <label for="sink-stripe-value-key">Value key</label><input
                  id="sink-stripe-value-key"
                  class="mono"
                  bind:value={sinkStripeValueKey}
                  placeholder="olp_value"
                />
              </div>
            {/if}
          {/if}
        </div>
        <div class="form-actions">
          {#if editingSink}<button
              class="button button-secondary"
              type="button"
              onclick={cancelSinkEdit}>Cancel</button
            >{/if}
          <button
            class="button button-primary"
            type="submit"
            disabled={sinkBusy ||
              !sinkName.trim() ||
              !sinkDestination.trim() ||
              (!editingSink && !sinkStreams.length)}
            >{sinkBusy
              ? 'Saving…'
              : editingSink
                ? 'Save sink'
                : 'Create sink'}</button
          >
        </div>
      </fieldset>
    </form>
  {/if}

  {#if sinks.isPending}<span class="inline-status" role="status"
      >Loading sinks…</span
    >
  {:else if sinks.isError}<span class="inline-problem" role="alert"
      >Sinks are unavailable.
      <button class="text-button" type="button" onclick={() => sinks.refetch()}
        >Retry</button
      ></span
    >
  {:else if !(sinks.data ?? []).length}<p class="section-help">
      No export sinks yet.
    </p>
  {:else}
    <div class="table-scroll">
      <table class="data-table">
        <thead
          ><tr
            ><th scope="col">Name</th><th scope="col">Type</th><th scope="col"
              >Project</th
            ><th scope="col">Streams</th><th scope="col">Delivery</th><th
              scope="col">Enabled</th
            ><th scope="col"><span class="sr-only">Actions</span></th></tr
          ></thead
        >
        <tbody>
          {#each sinks.data ?? [] as sink (sink.export_sink_id)}
            <tr
              ><td
                ><strong>{sink.name}</strong><br /><small class="mono"
                  >{sink.destination}</small
                >{#if sinkSettingsSummary(sink)}<br /><small
                    >{sinkSettingsSummary(sink)}</small
                  >{/if}</td
              ><td>{sink.type} · {sink.format}</td><td
                >{sink.project_id ? 'Project' : 'Installation-wide'}</td
              ><td>{sink.streams.join(', ')}</td><td
                >{#each streamStatuses(sink) as st (st.stream)}
                  <div class="stream-status">
                    <span class="mono">{st.stream}</span> — {streamState(st)}:
                    {st.pending ?? 0} pending, {st.delivered_total ?? 0} delivered,
                    {st.failures_total ?? 0} failed, {st.gaps_total ?? 0} gaps{#if (st.pending_error_codes ?? []).length}<br
                      /><small
                        >errors: {st.pending_error_codes!.join(', ')}</small
                      >{/if}{#if (st.pending ?? 0) > 0}<br /><small
                        >{st.oldest_pending_at &&
                        Number.isFinite(Date.parse(st.oldest_pending_at))
                          ? `approx. lag ${Math.round(lagSeconds(st))}s`
                          : 'lag unknown'}</small
                      >{/if}{#if st.last_success_at}<br /><small
                        >last success {formatDate(st.last_success_at)}</small
                      >{/if}{#if st.last_attempt_at}<br /><small
                        >last attempt {formatDate(st.last_attempt_at)}</small
                      >{/if}
                  </div>
                {/each}</td
              ><td>{sink.enabled ? 'Yes' : 'No'}</td><td
                ><button
                  class="text-button"
                  type="button"
                  onclick={() => toggleGaps(sink)}>Gaps</button
                >{#if sink.format === 'csv_gzip'}<button
                    class="text-button"
                    type="button"
                    onclick={() => toggleDatasets(sink)}>Datasets</button
                  >{/if}{#if canMutate(sink.project_id)}<button
                    class="text-button"
                    type="button"
                    disabled={sinkBusy}
                    onclick={() => startSinkEdit(sink)}>Edit</button
                  ><button
                    class="text-button"
                    type="button"
                    onclick={() => toggleSink(sink)}
                    >{sink.enabled ? 'Disable' : 'Enable'}</button
                  ><button
                    class="text-button"
                    type="button"
                    onclick={() => removeSink(sink)}>Delete</button
                  >{/if}</td
              ></tr
            >
            {#if gapSinkId === sink.export_sink_id}
              <tr class="gap-row"
                ><td colspan="7"
                  >{#if gapQuery.isPending}<span class="inline-status"
                      >Loading gaps…</span
                    >{:else if gapQuery.isError}<span class="inline-problem"
                      >Gap records are unavailable.</span
                    >{:else if !(gapQuery.data ?? []).length}<span
                      class="section-help">No export gaps recorded.</span
                    >{:else}<ul class="gap-list">
                      {#each gapQuery.data ?? [] as gap (gap.id)}<li>
                          <span class="mono">{gap.stream}</span> — {gap.record_count}
                          records, {formatDate(gap.first_occurred_at)} → {formatDate(
                            gap.last_occurred_at
                          )}: {gap.reason}
                        </li>{/each}
                    </ul>{/if}</td
                ></tr
              >
            {/if}
            {#if datasetSinkId === sink.export_sink_id}
              <tr class="gap-row"
                ><td colspan="7"
                  >{#if focusDatasets.isPending}<span class="inline-status"
                      >Loading datasets…</span
                    >{:else if focusDatasets.isError}<span
                      class="inline-problem"
                      >Focus datasets are unavailable.</span
                    >{:else if !(focusDatasets.data ?? []).length}<span
                      class="section-help">No datasets delivered yet.</span
                    >{:else}<table class="data-table focus-datasets">
                      <thead
                        ><tr
                          ><th scope="col">Charge date</th><th scope="col"
                            >Revision</th
                          ><th scope="col">Delivered</th><th scope="col"
                            >Rows</th
                          ><th scope="col">Billed cost</th><th scope="col"
                            >Commitments</th
                          ><th scope="col">Status</th></tr
                        ></thead
                      ><tbody>
                        {#each focusDatasets.data ?? [] as dataset (dataset.charge_date)}
                          <tr
                            ><td class="mono">{dataset.charge_date}</td><td
                              >{dataset.revision}</td
                            ><td
                              >{dataset.delivered_at
                                ? formatDate(dataset.delivered_at)
                                : 'pending'}</td
                            ><td>{dataset.row_count}</td><td
                              >{formatCost(dataset.billed_cost, null)}</td
                            ><td
                              >{#if dataset.commitment_totals}
                                <div>
                                  Purchased: {dataset.commitment_totals
                                    .purchased_cost}
                                </div>
                                <div>
                                  Allocated: {dataset.commitment_totals
                                    .allocated_cost}
                                </div>
                                <div>
                                  Unused: {dataset.commitment_totals
                                    .unused_cost}
                                </div>
                                <div>
                                  {dataset.commitment_totals.provisional
                                    ? 'Provisional'
                                    : 'Closed'} · {dataset.commitment_totals
                                    .complete
                                    ? 'Complete'
                                    : 'Incomplete coverage'}
                                </div>
                              {:else}Unavailable{/if}</td
                            ><td>{focusDatasetStatus(dataset)}</td></tr
                          >
                        {/each}
                      </tbody>
                    </table>{/if}</td
                ></tr
              >
            {/if}
          {/each}
        </tbody>
      </table>
    </div>
  {/if}

  <h3>Payload capture</h3>
  <div class="capture-warning" role="note">
    Payload capture stores <strong>unredacted</strong> request and response content
    — including prompts, completions, tool calls and any secrets callers paste. It
    is opt-in, queues are volatile, and delivery reaches only the configured operator
    sink. Active redaction is not available in this release; do not treat capture
    as filtered.
  </div>
  {#if captureConfig.isPending}<span class="inline-status" role="status"
      >Loading capture…</span
    >
  {:else if captureConfig.isError}<span class="inline-problem" role="alert"
      >Capture settings are unavailable.
      <button
        class="text-button"
        type="button"
        onclick={() => captureConfig.refetch()}>Retry</button
      ></span
    >
  {:else if captureConfig.data}
    <p class="section-help">
      Installation capture is {captureConfig.data.enabled
        ? 'enabled'
        : 'disabled'}.
      {#if ownerOnly}
        <button class="text-button" type="button" onclick={toggleMaster}
          >{captureConfig.data.enabled
            ? 'Disable capture'
            : 'Enable capture'}</button
        >
      {:else}<span>Only the installation owner can change this.</span>{/if}
    </p>
  {/if}

  {#if canManageProjects || canManageInstallation}
    <form class="create-form" onsubmit={submitPolicy}>
      <fieldset class="submission-fields" disabled={policyBusy}>
        <div class="form-grid">
          <ProjectScopeField
            id="policy-project"
            bind:value={policyProjectId}
            unassigned={canManageInstallation}
            disabled={!!editingPolicy}
          />
          <div class="form-field">
            <label for="policy-route">Route</label><input
              id="policy-route"
              bind:value={policyRoute}
              placeholder="Optional route slug"
              disabled={!!editingPolicy}
            />
          </div>
          <div class="form-field">
            <label for="policy-sink">Sink</label><select
              id="policy-sink"
              bind:value={policySinkId}
              required
              ><option value="" disabled>Choose an installation sink</option
              >{#each sinkOptions.data ?? [] as option (option.export_sink_id)}<option
                  value={option.export_sink_id}
                  >{option.name} ({option.type})</option
                >{/each}</select
            >
          </div>
          <div class="form-field">
            <label for="policy-ratio">Sample ratio</label><input
              id="policy-ratio"
              bind:value={policyRatio}
              required
              placeholder="0.25"
            />
          </div>
          <fieldset class="form-field">
            <legend>Include</legend>
            {#each ['input', 'output', 'tool_calls'] as part (part)}
              <label class="inline-check"
                ><input
                  type="checkbox"
                  checked={policyInclude.includes(part)}
                  onchange={() => toggleInclude(part)}
                />{part}</label
              >
            {/each}
          </fieldset>
          <div class="form-field">
            <label for="policy-keys">Key IDs</label><input
              id="policy-keys"
              bind:value={policyKeyIds}
              placeholder="Optional comma-separated UUIDs"
            />
          </div>
          <div class="form-field">
            <label for="policy-users">End-user digests</label><input
              id="policy-users"
              bind:value={policyEndUsers}
              placeholder="Optional comma-separated digests"
            />
          </div>
          <div class="form-field">
            <label for="policy-max">Max bytes</label><input
              id="policy-max"
              inputmode="numeric"
              bind:value={policyMaxBytes}
              required
            />
          </div>
          <div class="form-field">
            <label class="inline-check"
              ><input
                type="checkbox"
                bind:checked={policyEnabled}
              />Enabled</label
            >
          </div>
        </div>
        <div class="form-actions">
          {#if editingPolicy}<button
              class="button button-secondary"
              type="button"
              onclick={cancelPolicyEdit}>Cancel</button
            >{/if}
          <button
            class="button button-primary"
            type="submit"
            disabled={policyBusy ||
              !policySinkId ||
              !policyRatio.trim() ||
              !policyInclude.length}
            >{policyBusy
              ? 'Saving…'
              : editingPolicy
                ? 'Save capture policy'
                : 'Create capture policy'}</button
          >
        </div>
      </fieldset>
    </form>
  {/if}

  {#if policies.isPending}<span class="inline-status" role="status"
      >Loading capture policies…</span
    >
  {:else if policies.isError}<span class="inline-problem" role="alert"
      >Capture policies are unavailable.
      <button
        class="text-button"
        type="button"
        onclick={() => policies.refetch()}>Retry</button
      ></span
    >
  {:else if !(policies.data ?? []).length}<p class="section-help">
      No capture policies yet.
    </p>
  {:else}
    <div class="table-scroll">
      <table class="data-table">
        <thead
          ><tr
            ><th scope="col">Scope</th><th scope="col">Sink</th><th scope="col"
              >Ratio</th
            ><th scope="col">Includes</th><th scope="col">Enabled</th><th
              scope="col"><span class="sr-only">Actions</span></th
            ></tr
          ></thead
        >
        <tbody>
          {#each policies.data ?? [] as policy (policy.id)}
            <tr
              ><td
                >{policy.project_id
                  ? 'Project'
                  : 'Installation'}{policy.route_slug
                  ? ` · ${policy.route_slug}`
                  : ''}</td
              ><td>{sinkOptionLabel(policy.sink)}</td><td
                ><span class="mono">{policy.sample_ratio}</span></td
              ><td>{policy.include.join(', ')}</td><td
                >{policy.enabled ? 'Yes' : 'No'}</td
              ><td
                >{#if canMutate(policy.project_id)}<button
                    class="text-button"
                    type="button"
                    disabled={policyBusy}
                    onclick={() => startPolicyEdit(policy)}>Edit</button
                  ><button
                    class="text-button"
                    type="button"
                    onclick={() => togglePolicy(policy)}
                    >{policy.enabled ? 'Disable' : 'Enable'}</button
                  ><button
                    class="text-button"
                    type="button"
                    onclick={() => removePolicy(policy)}>Delete</button
                  >{/if}</td
              ></tr
            >
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

<style>
  .observability-panel {
    display: grid;
    gap: 1rem;
    margin-top: 1.5rem;
    padding: 1.5rem;
  }
  .observability-panel h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .observability-panel h3 {
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
  .submission-fields {
    display: grid;
    gap: 1rem;
    border: 0;
    padding: 0;
    margin: 0;
    min-width: 0;
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
    gap: 0.5rem;
  }
  .table-scroll {
    overflow-x: auto;
  }
  .table-scroll th {
    position: relative;
  }
  .capture-warning {
    padding: 0.75rem 1rem;
    border-radius: var(--radius-control);
    border: 1px solid var(--warning);
    color: var(--warning);
    font-size: 0.8rem;
  }
  .stream-status {
    font-size: var(--text-caption);
  }
  .gap-list {
    margin: 0;
    padding-left: 1rem;
    font-size: 0.8rem;
    color: var(--foreground-muted);
  }
  .inline-check {
    display: flex;
    align-items: center;
    gap: 0.35rem;
    font-size: var(--text-caption);
    color: var(--foreground-muted);
  }
  td small {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .customer-row {
    display: flex;
    gap: 0.5rem;
    align-items: center;
    margin-bottom: 0.4rem;
  }
  .customer-row select,
  .customer-row input {
    flex: 1;
    min-width: 0;
  }
</style>
