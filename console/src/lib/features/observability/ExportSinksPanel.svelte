<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/management/http';
  import ProjectScopeField from '$lib/features/access/projects/ProjectScopeField.svelte';
  import { observabilityKeys } from './observabilityKeys';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '$lib/editing/mutationIntent.svelte';
  import type { ProjectMembership } from '$lib/features/access/projects/api';
  import { formatDate } from '$lib/format';
  import ExportSinkHistory from './ExportSinkHistory.svelte';
  import {
    createExportSink,
    deleteExportSink,
    listExportSinks,
    updateExportSink,
    type ExportSink
  } from './api';
  import {
    newSinkDelivery,
    changeSinkDelivery,
    editSinkDelivery,
    toggleSinkStream,
    parseSinkSettings,
    parseSinkCredential,
    type SinkDelivery
  } from './sinkSettings';
  import {
    lagSeconds,
    sinkSettingsSummary,
    sinkStreamOptions,
    sinkTypeFormats,
    isSinkType,
    sinkTypeLabels,
    streamState,
    streamStatuses,
    stripeMeterValues
  } from './presentation';
  const queryClient = useQueryClient();
  let error = $state('');
  let notice = $state('');
  const sinkIntent = useMutationIntent();
  let {
    canManageInstallation,
    canManageProjects,
    canMutate,
    memberships
  }: {
    canManageInstallation: boolean;
    canManageProjects: boolean;
    canMutate: (project: string | null | undefined) => boolean;
    memberships: ProjectMembership[];
  } = $props();
  const sinks = createQuery(() => ({
    queryKey: observabilityKeys.sinks(),
    queryFn: ({ signal }) => listExportSinks(signal)
  }));
  let editingSink = $state<ExportSink | null>(null);
  let sinkName = $state('');
  let delivery = $state<SinkDelivery>(newSinkDelivery('https'));
  let sinkDestination = $state('');
  let sinkProjectId = $state('');
  let sinkFilterProject = $state('');
  let sinkFilterRoute = $state('');
  let sinkFilterOutcome = $state<'' | 'success' | 'failure'>('');
  let sinkCredential = $state('');
  let sinkClearCredential = $state(false);
  let sinkStripeKey = $state('');
  let sinkStripeAdvanced = $state(false);
  let sinkBusy = $state(false);

  const streamLock = $derived(
    delivery.format === 'cloudevents' || delivery.format === 'stripe'
      ? ['metering']
      : delivery.format === 'csv_gzip'
        ? ['focus']
        : null
  );
  const focusVisible = $derived(delivery.format === 'csv_gzip');
  const outcomeVisible = $derived(delivery.format !== 'csv_gzip');
  function selectDelivery(
    type: ExportSink['type'],
    format?: ExportSink['format']
  ) {
    delivery = changeSinkDelivery(delivery, type, format);
    if (type === 'stripe_meter' && !sinkDestination.trim())
      sinkDestination = 'https://api.stripe.com';
  }
  function toggleStream(stream: ExportSink['streams'][number]) {
    delivery = toggleSinkStream(delivery, stream);
  }

  function removeCustomer(index: number) {
    if (delivery.format !== 'stripe') return;
    delivery = {
      ...delivery,
      customers: delivery.customers.filter((_, position) => position !== index)
    };
  }
  function addCustomer() {
    if (delivery.format !== 'stripe') return;
    delivery = {
      ...delivery,
      customers: [...delivery.customers, { project: '', customer: '' }]
    };
  }

  function sinkFilterValue() {
    const filter = {
      ...(sinkFilterProject ? { project: sinkFilterProject } : {}),
      ...(sinkFilterRoute.trim() ? { route: sinkFilterRoute.trim() } : {}),
      ...(sinkFilterOutcome && outcomeVisible
        ? { outcome: sinkFilterOutcome }
        : {})
    };
    return Object.keys(filter).length ? filter : null;
  }

  function startSinkEdit(sink: ExportSink) {
    if (sinkBusy) return;
    sinkIntent.reset();
    editingSink = sink;
    sinkName = sink.name;
    delivery = editSinkDelivery(sink);
    sinkDestination = sink.destination;
    sinkProjectId = sink.project_id ?? '';
    const filter = sink.filter ?? {};
    sinkFilterProject = filter.project ?? '';
    sinkFilterRoute = filter.route ?? '';
    sinkFilterOutcome = filter.outcome ?? '';
    sinkCredential = '';
    sinkClearCredential = false;
    sinkStripeKey = '';
    sinkStripeAdvanced =
      delivery.format === 'stripe' &&
      Boolean(delivery.stripeCustomerKey || delivery.stripeValueKey);
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
    delivery = changeSinkDelivery(delivery, delivery.type, delivery.format);
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
      const parsed = parseSinkSettings(delivery, {
        editing: Boolean(editing),
        stripeKey: sinkStripeKey
      });
      if (parsed.kind === 'invalid') {
        error = parsed.message;
        return;
      }
      const settings = parsed.settings;
      const credential = parseSinkCredential({
        clear: sinkClearCredential,
        type: delivery.type,
        stripeKey: sinkStripeKey,
        json: sinkCredential
      });
      if (credential.kind === 'invalid') {
        error = credential.message;
        return;
      }
      const credentialFields =
        credential.change.kind === 'keep'
          ? {}
          : {
              credential:
                credential.change.kind === 'clear'
                  ? null
                  : credential.change.value
            };
      if (editing) {
        await updateExportSink(editing, {
          name: sinkName.trim(),
          destination: sinkDestination.trim(),
          filter: sinkFilterValue(),
          ...(settings ? { settings } : {}),
          ...credentialFields
        });
        committed = true;
        if (!isCurrent()) return;
        notice = 'Sink updated.';
      } else {
        if (!delivery.streams.length) return;
        const submission = sinkIntent.prepare<
          Parameters<typeof createExportSink>[0]
        >({
          name: sinkName.trim(),
          type: delivery.type,
          destination: sinkDestination.trim(),
          project_id: sinkProjectId || null,
          streams: delivery.streams,
          format: delivery.format,
          filter: sinkFilterValue(),
          settings,
          ...credentialFields
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

  let gapSinkId = $state('');
  function toggleGaps(sink: ExportSink) {
    gapSinkId = gapSinkId === sink.export_sink_id ? '' : sink.export_sink_id;
  }
  let datasetSinkId = $state('');
  function toggleDatasets(sink: ExportSink) {
    datasetSinkId =
      datasetSinkId === sink.export_sink_id ? '' : sink.export_sink_id;
  }

  const captureOwner = usePrincipalLifetime(() => {
    resetSinkForm();
    sinkBusy = false;
    error = notice = '';
  });
</script>

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
            value={delivery.type}
            onchange={(event) => {
              const type = event.currentTarget.value;
              if (isSinkType(type)) selectDelivery(type);
            }}
            disabled={!!editingSink}
            >{#each Object.keys(sinkTypeFormats) as type (type)}<option
                value={type}>{sinkTypeLabels[type] ?? type}</option
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
            value={delivery.format}
            onchange={(event) => {
              const option = sinkTypeFormats[delivery.type].find(
                (option) => option.value === event.currentTarget.value
              );
              if (option) selectDelivery(delivery.type, option.value);
            }}
            disabled={!!editingSink}
            >{#each sinkTypeFormats[delivery.type] ?? [] as option (option.value)}<option
                value={option.value}>{option.label}</option
              >{/each}</select
          >
        </div>
        <fieldset class="form-field">
          <legend>Streams</legend>
          {#each sinkStreamOptions as stream (stream.value)}
            {#if stream.value !== 'focus' || focusVisible}
              <label class="inline-check"
                ><input
                  type="checkbox"
                  checked={delivery.streams.some(
                    (value) => value === stream.value
                  )}
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
        {#if delivery.type === 'stripe_meter'}
          <div class="form-field">
            <label for="sink-stripe-key">Restricted API key</label><input
              id="sink-stripe-key"
              type="password"
              autocomplete="off"
              bind:value={sinkStripeKey}
              placeholder={editingSink ? 'Blank keeps the stored key' : 'rk_…'}
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
        {#if delivery.format === 'csv_gzip'}
          <div class="form-field">
            <label for="sink-cost-basis">FOCUS cost basis</label><select
              id="sink-cost-basis"
              bind:value={delivery.focusCostBasis}
              disabled={!!editingSink}
              ><option value="cost">Provider cost</option><option value="charge"
                >Charge</option
              ></select
            >{#if editingSink}<small
                >Cost basis cannot change after creation.</small
              >{/if}
          </div>
          <div class="form-field">
            <label for="sink-focus-start">FOCUS start date</label><input
              id="sink-focus-start"
              type="date"
              bind:value={delivery.focusStartDate}
            /><small
              >Optional, new dates must be within the retained usage window.</small
            >
          </div>
        {:else if delivery.format === 'cloudevents'}
          <div class="form-field">
            <label for="sink-ce-source">CloudEvents source</label><input
              id="sink-ce-source"
              bind:value={delivery.ceSource}
              placeholder="https://example.com/olp"
            />
          </div>
        {:else if delivery.format === 'stripe'}
          <div class="form-field">
            <label for="sink-stripe-event">Stripe event name</label><input
              id="sink-stripe-event"
              bind:value={delivery.stripeEvent}
              required
              placeholder="olp.charge"
            />
          </div>
          <div class="form-field">
            <label for="sink-stripe-value">Meter value</label><select
              id="sink-stripe-value"
              bind:value={delivery.stripeValue}
              >{#each stripeMeterValues as option (option.value)}<option
                  value={option.value}>{option.label}</option
                >{/each}</select
            >
          </div>
          <fieldset class="form-field">
            <legend>Stripe customers</legend>
            {#each delivery.customers as row, i (i)}
              <div class="customer-row">
                <select aria-label="Project" bind:value={row.project}
                  >{#each memberships as membership (membership.id)}<option
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
                  disabled={delivery.customers.length <= 1}
                  onclick={() => removeCustomer(i)}>&times;</button
                >
              </div>
            {/each}
            <button class="text-button" type="button" onclick={addCustomer}
              >Add mapping</button
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
                bind:value={delivery.stripeCustomerKey}
                placeholder="olp_project"
              />
            </div>
            <div class="form-field">
              <label for="sink-stripe-value-key">Value key</label><input
                id="sink-stripe-value-key"
                class="mono"
                bind:value={delivery.stripeValueKey}
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
            (!editingSink && !delivery.streams.length)}
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
                {@const errorCodes = st.pending_error_codes ?? []}
                <div class="stream-status">
                  <span class="mono">{st.stream}</span> — {streamState(st)}:
                  {st.pending ?? 0} pending, {st.delivered_total ?? 0} delivered,
                  {st.failures_total ?? 0} failed, {st.gaps_total ?? 0} gaps{#if errorCodes.length}<br
                    /><small>errors: {errorCodes.join(', ')}</small
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
                ><ExportSinkHistory
                  sinkId={sink.export_sink_id}
                  kind="gaps"
                /></td
              ></tr
            >
          {/if}
          {#if datasetSinkId === sink.export_sink_id}
            <tr class="gap-row"
              ><td colspan="7"
                ><ExportSinkHistory
                  sinkId={sink.export_sink_id}
                  kind="datasets"
                /></td
              ></tr
            >
          {/if}
        {/each}
      </tbody>
    </table>
  </div>
{/if}
