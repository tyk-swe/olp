<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { holds } from '$lib/features/access/session/authorization';
  import ProjectScopeField from '$lib/features/access/projects/ProjectScopeField.svelte';
  import { listProjectMemberships } from '$lib/features/access/projects/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
  import { formatDate } from '$lib/format';
  import {
    createCapturePolicy,
    createExportSink,
    deleteCapturePolicy,
    deleteExportSink,
    getCaptureConfiguration,
    listCapturePolicies,
    listCaptureSinkOptions,
    listExportSinks,
    listExportSinkGaps,
    updateCaptureConfiguration,
    updateCapturePolicy,
    updateExportSink,
    type CapturePolicy,
    type ExportSink
  } from '$lib/features/observability/api';
  import { observabilityKeys } from '$lib/features/observability/observabilityKeys';

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
    https: [{ value: 'json', label: 'JSON' }],
    otlp_logs: [{ value: 'otlp', label: 'OTLP logs' }],
    s3: [{ value: 'jsonl', label: 'JSON Lines' }],
    gcs: [{ value: 'jsonl', label: 'JSON Lines' }],
    azure_blob: [{ value: 'jsonl', label: 'JSON Lines' }]
  };
  const streamOptions = [
    { value: 'requests', label: 'Requests' },
    { value: 'attempts', label: 'Attempts' },
    { value: 'usage_rollups', label: 'Usage rollups' },
    { value: 'guardrail_decisions', label: 'Guardrail decisions' },
    { value: 'audit', label: 'Audit' }
  ];

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
  let sinkBusy = $state(false);

  $effect(() => {
    const formats = typeFormats[sinkType] ?? typeFormats.https;
    if (!formats.some((f) => f.value === sinkFormat))
      sinkFormat = formats[0].value;
  });

  function toggleStream(stream: string) {
    sinkStreams = sinkStreams.includes(stream)
      ? sinkStreams.filter((value) => value !== stream)
      : [...sinkStreams, stream];
  }

  function sinkFilterValue() {
    const filter: Record<string, unknown> = {};
    if (sinkFilterProject) filter.project = sinkFilterProject;
    if (sinkFilterRoute.trim()) filter.route = sinkFilterRoute.trim();
    if (sinkFilterOutcome) filter.outcome = sinkFilterOutcome;
    return Object.keys(filter).length ? filter : null;
  }

  function startSinkEdit(sink: ExportSink) {
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
    error = '';
  }

  function resetSinkForm() {
    editingSink = null;
    sinkName = '';
    sinkDestination = '';
    sinkFilterRoute = '';
    sinkFilterOutcome = '';
    sinkFilterProject = '';
    sinkCredential = '';
    sinkClearCredential = false;
  }

  async function submitSink(event: SubmitEvent) {
    event.preventDefault();
    const editing = editingSink;
    const allowed = editing
      ? canMutate(editing.project_id)
      : canMutate(sinkProjectId || null);
    if (!allowed || sinkBusy || !sinkName.trim() || !sinkDestination.trim())
      return;
    sinkBusy = true;
    error = notice = '';
    try {
      let credential: unknown;
      if (sinkCredential.trim()) {
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
          ...(sinkClearCredential
            ? { credential: null }
            : credential !== undefined
              ? { credential: credential as Record<string, unknown> }
              : {})
        });
        notice = 'Sink updated.';
      } else {
        if (!sinkStreams.length) return;
        await createExportSink({
          name: sinkName.trim(),
          type: sinkType,
          destination: sinkDestination.trim(),
          project_id: sinkProjectId || null,
          streams: sinkStreams as ExportSink['streams'],
          format: sinkFormat as ExportSink['format'],
          filter: sinkFilterValue(),
          ...(credential !== undefined
            ? { credential: credential as Record<string, unknown> }
            : {})
        });
        notice = 'Sink created.';
      }
      resetSinkForm();
      await queryClient.invalidateQueries({
        queryKey: observabilityKeys.root
      });
    } catch (cause) {
      error = errorMessage(
        cause,
        editing
          ? 'The sink could not be updated.'
          : 'The sink could not be created.'
      );
    } finally {
      sinkBusy = false;
    }
  }

  async function toggleSink(sink: ExportSink) {
    if (!canMutate(sink.project_id)) return;
    error = notice = '';
    try {
      await updateExportSink(sink, { enabled: !sink.enabled });
      await queryClient.invalidateQueries({ queryKey: observabilityKeys.root });
    } catch (cause) {
      error = errorMessage(cause, 'The sink could not be updated.');
    }
  }

  async function removeSink(sink: ExportSink) {
    if (!canMutate(sink.project_id)) return;
    error = notice = '';
    try {
      await deleteExportSink(sink);
      notice = 'Sink deleted.';
      await queryClient.invalidateQueries({ queryKey: observabilityKeys.root });
    } catch (cause) {
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
    editingPolicy = null;
    policyRoute = policyKeyIds = policyEndUsers = '';
    policySinkId = '';
    policyRatio = '1';
    policyEnabled = true;
  }

  async function toggleMaster() {
    if (!ownerOnly || !captureConfig.data) return;
    error = notice = '';
    try {
      await updateCaptureConfiguration(captureConfig.data, {
        enabled: !captureConfig.data.enabled
      });
      await queryClient.invalidateQueries({
        queryKey: observabilityKeys.capture()
      });
    } catch (cause) {
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
    policyBusy = true;
    error = notice = '';
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
        notice = 'Capture policy updated.';
      } else {
        await createCapturePolicy({
          project_id: policyProjectId || null,
          route_slug: policyRoute.trim() || null,
          ...fields
        });
        notice = 'Capture policy created.';
      }
      resetPolicyForm();
      await queryClient.invalidateQueries({
        queryKey: observabilityKeys.capturePolicies()
      });
    } catch (cause) {
      error = errorMessage(
        cause,
        editing
          ? 'The capture policy could not be updated.'
          : 'The capture policy could not be created.'
      );
    } finally {
      policyBusy = false;
    }
  }

  async function togglePolicy(policy: CapturePolicy) {
    if (!canMutate(policy.project_id)) return;
    error = notice = '';
    try {
      await updateCapturePolicy(policy, { enabled: !policy.enabled });
      await queryClient.invalidateQueries({
        queryKey: observabilityKeys.capturePolicies()
      });
    } catch (cause) {
      error = errorMessage(cause, 'The capture policy could not be updated.');
    }
  }

  async function removePolicy(policy: CapturePolicy) {
    if (!canMutate(policy.project_id)) return;
    error = notice = '';
    try {
      await deleteCapturePolicy(policy);
      notice = 'Capture policy deleted.';
      await queryClient.invalidateQueries({
        queryKey: observabilityKeys.capturePolicies()
      });
    } catch (cause) {
      error = errorMessage(cause, 'The capture policy could not be deleted.');
    }
  }

  type SinkStreamStatus = {
    stream?: string;
    pending?: number;
    pending_error_codes?: string[];
    delivered_total?: number;
    failures_total?: number;
    gaps_total?: number;
    last_success_at?: string | null;
    last_attempt_at?: string | null;
    oldest_pending_at?: string | null;
  };

  function streamStatuses(sink: ExportSink): SinkStreamStatus[] {
    return (sink.status ?? []) as SinkStreamStatus[];
  }

  function lagSeconds(status: SinkStreamStatus): number {
    return status.oldest_pending_at &&
      Number.isFinite(Date.parse(status.oldest_pending_at))
      ? Math.max(0, (Date.now() - Date.parse(status.oldest_pending_at)) / 1000)
      : 0;
  }

  function streamState(status: SinkStreamStatus): string {
    if (status.pending == null) return 'unknown';
    if (status.pending > 0)
      return (status.pending_error_codes ?? []).length > 0
        ? 'failing'
        : 'pending';
    return 'idle';
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
            >{#each Object.keys(typeFormats) as type (type)}<option value={type}
                >{type}</option
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
            <label class="inline-check"
              ><input
                type="checkbox"
                checked={sinkStreams.includes(stream.value)}
                disabled={!!editingSink}
                onchange={() => toggleStream(stream.value)}
              />{stream.label}</label
            >
          {/each}
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
        <div class="form-field">
          <label for="sink-filter-outcome">Outcome filter</label><select
            id="sink-filter-outcome"
            bind:value={sinkFilterOutcome}
            ><option value="">All outcomes</option><option value="success"
              >Success</option
            ><option value="failure">Failure</option></select
          >
        </div>
        <div class="form-field">
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
        </div>
      </div>
      <div class="form-actions">
        {#if editingSink}<button
            class="button button-secondary"
            type="button"
            onclick={resetSinkForm}>Cancel</button
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
                ></td
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
                >{#if canMutate(sink.project_id)}<button
                    class="text-button"
                    type="button"
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
            onclick={resetPolicyForm}>Cancel</button
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
</style>
