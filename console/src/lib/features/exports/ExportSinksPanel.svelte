<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import type { ProjectMembership } from '#lib/domain/projects/api.js';
  import { observabilityKeys } from './observabilityKeys';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import ExportSinkDialog from './ExportSinkDialog.svelte';
  import ExportSinkList from './ExportSinkList.svelte';
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
    parseSinkSettings,
    parseSinkCredential,
    type SinkDelivery
  } from './sinkSettings';
  import type { SinkOutcome } from './presentation';
  const queryClient = useQueryClient();
  let creating = $state(false);
  let error = $state('');
  const sinkIntent = useMutationIntent();
  const saveAction = useAction();
  const rowAction = useAction();
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
  let sinkFilterOutcome = $state<'' | SinkOutcome>('');
  let sinkCredential = $state('');
  let sinkClearCredential = $state(false);
  let sinkStripeKey = $state('');
  let sinkStripeAdvanced = $state(false);

  const outcomeVisible = $derived(delivery.format !== 'csv_gzip');

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
    if (saveAction.busy) return;
    sinkIntent.reset();
    editingSink = sink;
    creating = true;
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
    creating = false;
    if (!saveAction.busy) resetSinkForm();
  }

  async function submitSink(event: SubmitEvent) {
    event.preventDefault();
    const editing = editingSink;
    const allowed = editing
      ? canMutate(editing.project_id)
      : canMutate(sinkProjectId || null);
    if (
      !allowed ||
      saveAction.busy ||
      !sinkName.trim() ||
      !sinkDestination.trim()
    )
      return;
    error = '';
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
    if (!editing && !delivery.streams.length) return;
    const credentialFields =
      credential.change.kind === 'keep'
        ? {}
        : {
            credential:
              credential.change.kind === 'clear'
                ? null
                : credential.change.value
          };
    await saveAction.run(
      (): Promise<unknown> =>
        editing
          ? updateExportSink(editing, {
              name: sinkName.trim(),
              destination: sinkDestination.trim(),
              filter: sinkFilterValue(),
              ...(settings ? { settings } : {}),
              ...credentialFields
            })
          : delivery.streams.length
            ? sinkIntent.submit(
                {
                  name: sinkName.trim(),
                  type: delivery.type,
                  destination: sinkDestination.trim(),
                  project_id: sinkProjectId || null,
                  streams: delivery.streams,
                  format: delivery.format,
                  filter: sinkFilterValue(),
                  settings,
                  ...credentialFields
                } satisfies Parameters<typeof createExportSink>[0],
                (input, requestId) => createExportSink(input, requestId)
              )
            : Promise.resolve(),
      async (_, current) => {
        creating = false;
        notify(editing ? 'Sink updated' : 'Sink created');
        resetSinkForm();
        await queryClient.invalidateQueries(
          {
            queryKey: observabilityKeys.root
          },
          { throwOnError: true }
        );
        if (!current()) return;
      },
      {
        key: 'save',
        failure: (cause, stage) =>
          stage === 'accept'
            ? `Saved, but the list could not be refreshed. ${errorMessage(cause)}`
            : errorMessage(
                cause,
                editing
                  ? 'The sink could not be updated.'
                  : 'The sink could not be created.'
              )
      }
    );
    error = saveAction.error;
  }

  async function toggleSink(sink: ExportSink) {
    if (!canMutate(sink.project_id)) return;
    error = '';
    await rowAction.run(
      () => updateExportSink(sink, { enabled: !sink.enabled }),
      async (_, current) => {
        await queryClient.invalidateQueries({
          queryKey: observabilityKeys.root
        });
        if (!current()) return;
      },
      {
        key: sink.export_sink_id,
        failure: (cause) =>
          errorMessage(cause, 'The sink could not be updated.')
      }
    );
    error = rowAction.error;
  }

  async function removeSink(sink: ExportSink) {
    if (!canMutate(sink.project_id)) return;
    if (
      !(await confirmAction({
        title: `Delete ${sink.name}?`,
        body: 'Records stop exporting to this destination.',
        confirmLabel: 'Delete sink',
        danger: true
      }))
    )
      return;
    error = '';
    await rowAction.run(
      () => deleteExportSink(sink),
      async (_, current) => {
        notify('Sink deleted.');
        await queryClient.invalidateQueries({
          queryKey: observabilityKeys.root
        });
        if (!current()) return;
      },
      {
        key: sink.export_sink_id,
        failure: (cause) =>
          errorMessage(cause, 'The sink could not be deleted.')
      }
    );
    error = rowAction.error;
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

  usePrincipalLifetime(() => {
    resetSinkForm();
    error = '';
  });
</script>

{#if error && !creating}<Callout tone="danger">{error}</Callout>{/if}

{#if creating}
  <ExportSinkDialog
    {editingSink}
    busy={saveAction.busy}
    {error}
    {canManageInstallation}
    {memberships}
    bind:sinkName
    bind:delivery
    bind:sinkDestination
    bind:sinkProjectId
    bind:sinkFilterProject
    bind:sinkFilterRoute
    bind:sinkFilterOutcome
    bind:sinkCredential
    bind:sinkClearCredential
    bind:sinkStripeKey
    bind:sinkStripeAdvanced
    onClose={() => {
      creating = false;
      cancelSinkEdit();
    }}
    onSubmit={submitSink}
    onCancel={cancelSinkEdit}
  />
{/if}

<ExportSinkList
  canCreate={canManageInstallation || canManageProjects}
  onCreate={() => {
    error = '';
    creating = true;
  }}
  pending={sinks.isPending}
  failed={sinks.isError}
  items={sinks.data ?? []}
  onRetry={() => sinks.refetch()}
  {canMutate}
  busy={saveAction.busy || rowAction.busy}
  bind:gapSinkId
  bind:datasetSinkId
  onEdit={startSinkEdit}
  onToggle={toggleSink}
  onGaps={toggleGaps}
  onDatasets={toggleDatasets}
  onRemove={removeSink}
/>
