<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { CAPTURE_REDACTOR_TYPES } from '#lib/management/enums.js';
  import { observabilityKeys } from './observabilityKeys';
  import CapturePolicyDialog from './CapturePolicyDialog.svelte';
  import CapturePolicyList from './CapturePolicyList.svelte';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import {
    createCapturePolicy,
    deleteCapturePolicy,
    getCaptureConfiguration,
    listCapturePolicies,
    listCaptureSinkOptions,
    updateCaptureConfiguration,
    updateCapturePolicy,
    type CapturePolicy
  } from './api';
  import { guardrailQueries } from '#lib/domain/guardrails/queries.js';
  const queryClient = useQueryClient();
  let creating = $state(false);
  let error = $state('');
  const policyIntent = useMutationIntent();
  const saveAction = useAction();
  const rowAction = useAction();
  const masterAction = useAction();
  let {
    canManageInstallation,
    canManageProjects,
    canMutate,
    ownerOnly
  }: {
    canManageInstallation: boolean;
    canManageProjects: boolean;
    canMutate: (project: string | null | undefined) => boolean;
    ownerOnly: boolean;
  } = $props();
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

  const redactorOptions = createQuery(() => ({
    ...guardrailQueries.definitions(),
    enabled: canManageProjects || canManageInstallation
  }));
  const redactorTypes: readonly string[] = CAPTURE_REDACTOR_TYPES;
  const eligibleRedactors = $derived(
    (redactorOptions.data ?? []).filter(
      (d) =>
        d.active_revision_id &&
        !d.retired_at &&
        redactorTypes.includes(d.type) &&
        (!policyProjectId || d.project_id === policyProjectId)
    )
  );

  let editingPolicy = $state<CapturePolicy | null>(null);
  let policyProjectId = $state('');
  let policyRoute = $state('');
  let policySinkId = $state('');
  let policyRatio = $state('1');
  let policyInclude = $state<string[]>([
    'input',
    'output'
  ] satisfies CapturePolicy['include']);
  let policyRedactors = $state<string[]>([]);
  let redactorName = $state('');
  let refreshRedactors = $state(false);
  let policyKeyIds = $state('');
  let policyEndUsers = $state('');
  let policyMaxBytes = $state('65536');
  let policyEnabled = $state(true);
  let policyExpiry = $state<string | null | undefined>();

  function startPolicyEdit(policy: CapturePolicy) {
    if (saveAction.busy) return;
    policyIntent.reset();
    editingPolicy = policy;
    creating = true;
    policyProjectId = policy.project_id ?? '';
    policyRoute = policy.route_slug ?? '';
    policySinkId = policy.sink;
    policyRatio = policy.sample_ratio;
    policyInclude = [...policy.include];
    policyRedactors = [...policy.redact];
    refreshRedactors = false;
    redactorName = '';
    policyKeyIds = policy.key_ids.join(', ');
    policyEndUsers = policy.end_user_digests.join(', ');
    policyMaxBytes = String(policy.max_bytes);
    policyEnabled = policy.enabled;
    policyExpiry = undefined;
    error = '';
  }

  function resetPolicyForm() {
    policyIntent.reset();
    editingPolicy = null;
    policyRedactors = [];
    refreshRedactors = false;
    redactorName = '';
    policyRoute = policyKeyIds = policyEndUsers = '';
    policySinkId = '';
    policyRatio = '1';
    policyEnabled = true;
    policyExpiry = undefined;
  }

  function cancelPolicyEdit() {
    creating = false;
    if (!saveAction.busy) resetPolicyForm();
  }

  usePrincipalLifetime(() => {
    resetPolicyForm();
    error = '';
  });

  async function toggleMaster() {
    if (!ownerOnly || !captureConfig.data) return;
    error = '';
    const config = captureConfig.data;
    await masterAction.run(
      () => updateCaptureConfiguration(config, { enabled: !config.enabled }),
      async (_, current) => {
        await queryClient.invalidateQueries({
          queryKey: observabilityKeys.capture()
        });
        if (!current()) return;
      },
      {
        key: 'master',
        failure: (cause) => errorMessage(cause, 'Capture could not be changed.')
      }
    );
    error = masterAction.error;
  }

  async function submitPolicy(event: SubmitEvent) {
    event.preventDefault();
    const editing = editingPolicy;
    const allowed = canMutate(
      editing ? editing.project_id : policyProjectId || null
    );
    if (
      !allowed ||
      saveAction.busy ||
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
    error = '';
    const fields = {
      ...(!editing ||
      refreshRedactors ||
      JSON.stringify(policyRedactors) !== JSON.stringify(editing.redact)
        ? { redact: [...policyRedactors] }
        : {}),
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
      enabled: policyEnabled,
      ...(policyExpiry !== undefined ? { expires_at: policyExpiry } : {})
    };
    await saveAction.run(
      () =>
        editing
          ? updateCapturePolicy(editing, fields)
          : policyIntent.submit(
              {
                project_id: policyProjectId || null,
                route_slug: policyRoute.trim() || null,
                ...fields
              } satisfies Parameters<typeof createCapturePolicy>[0],
              (input, requestId) => createCapturePolicy(input, requestId)
            ),
      async (_, current) => {
        creating = false;
        notify(editing ? 'Capture policy updated' : 'Capture policy created');
        resetPolicyForm();
        await queryClient.invalidateQueries(
          {
            queryKey: observabilityKeys.capturePolicies()
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
                  ? 'The capture policy could not be updated.'
                  : 'The capture policy could not be created.'
              )
      }
    );
    error = saveAction.error;
  }

  async function togglePolicy(policy: CapturePolicy) {
    if (!canMutate(policy.project_id)) return;
    error = '';
    await rowAction.run(
      () => updateCapturePolicy(policy, { enabled: !policy.enabled }),
      async (_, current) => {
        await queryClient.invalidateQueries({
          queryKey: observabilityKeys.capturePolicies()
        });
        if (!current()) return;
      },
      {
        key: policy.id,
        failure: (cause) =>
          errorMessage(cause, 'The capture policy could not be updated.')
      }
    );
    error = rowAction.error;
  }

  async function removePolicy(policy: CapturePolicy) {
    if (!canMutate(policy.project_id)) return;
    if (
      !(await confirmAction({
        title: 'Delete capture policy?',
        body: 'Matching prompts and responses stop being captured to this sink.',
        confirmLabel: 'Delete policy',
        danger: true
      }))
    )
      return;
    error = '';
    await rowAction.run(
      () => deleteCapturePolicy(policy),
      async (_, current) => {
        notify('Capture policy deleted.');
        await queryClient.invalidateQueries({
          queryKey: observabilityKeys.capturePolicies()
        });
        if (!current()) return;
      },
      {
        key: policy.id,
        failure: (cause) =>
          errorMessage(cause, 'The capture policy could not be deleted.')
      }
    );
    error = rowAction.error;
  }

  function sinkOptionLabel(id: string) {
    const option = (sinkOptions.data ?? []).find(
      (s) => s.export_sink_id === id
    );
    return option ? `${option.name} (${option.type})` : id;
  }
</script>

{#if error && !creating}<Callout tone="danger">{error}</Callout>{/if}

<div class="capture-warning" role="note">
  Capture is opt-in and delivered from memory to an installation-owned sink.
  Policies without redactors capture <strong>unredacted</strong> content. Configured
  local detectors inspect selected fields before enqueue; failed or incomplete inspection
  drops the whole capture. Detector coverage is bounded and does not guarantee removal
  of every secret or personal detail.
</div>
{#if captureConfig.isPending}<span class="inline-status" role="status">
    Loading capture…
  </span>
{:else if captureConfig.isError}<span class="inline-problem" role="alert">
    Capture settings are unavailable.
    <button
      class="text-button"
      type="button"
      onclick={() => captureConfig.refetch()}
    >
      Retry
    </button>
  </span>
{:else if captureConfig.data}
  <p class="section-help">
    Installation capture is {captureConfig.data.enabled
      ? 'enabled'
      : 'disabled'}.
    {#if ownerOnly}
      <button class="text-button" type="button" onclick={toggleMaster}>
        {captureConfig.data.enabled ? 'Disable capture' : 'Enable capture'}
      </button>
    {:else}<span>Only the installation owner can change this.</span>{/if}
  </p>
{/if}

{#if creating}
  <CapturePolicyDialog
    {editingPolicy}
    busy={saveAction.busy}
    {error}
    {canManageInstallation}
    sinkOptions={sinkOptions.data ?? []}
    {eligibleRedactors}
    bind:policyProjectId
    bind:policyRoute
    bind:policySinkId
    bind:policyRatio
    bind:policyInclude
    bind:policyRedactors
    bind:redactorName
    bind:policyKeyIds
    bind:policyEndUsers
    bind:policyMaxBytes
    bind:policyEnabled
    bind:policyExpiry
    bind:refreshRedactors
    onClose={() => {
      creating = false;
      cancelPolicyEdit();
    }}
    onSubmit={submitPolicy}
    onCancel={cancelPolicyEdit}
  />
{/if}

<CapturePolicyList
  canCreate={canManageProjects || canManageInstallation}
  onCreate={() => {
    error = '';
    creating = true;
  }}
  pending={policies.isPending}
  failed={policies.isError}
  items={policies.data ?? []}
  onRetry={() => policies.refetch()}
  {canMutate}
  busy={saveAction.busy || rowAction.busy}
  onEdit={startPolicyEdit}
  onToggle={togglePolicy}
  onRemove={removePolicy}
  {sinkOptionLabel}
/>
