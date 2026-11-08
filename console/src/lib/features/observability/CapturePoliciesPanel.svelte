<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import ProjectScopeField from '#lib/features/access/projects/ProjectScopeField.svelte';
  import { observabilityKeys } from './observabilityKeys';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import {
    createCapturePolicy,
    deleteCapturePolicy,
    getCaptureConfiguration,
    listCapturePolicies,
    listCaptureRedactorOptions,
    listCaptureSinkOptions,
    updateCaptureConfiguration,
    updateCapturePolicy,
    type CapturePolicy
  } from './api';
  const queryClient = useQueryClient();
  let error = $state('');
  let notice = $state('');
  const policyIntent = useMutationIntent();
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
    queryKey: observabilityKeys.redactorOptions(),
    queryFn: ({ signal }) => listCaptureRedactorOptions(signal),
    enabled: canManageProjects || canManageInstallation
  }));
  const eligibleRedactors = $derived(
    (redactorOptions.data ?? []).filter(
      (d) =>
        d.active_revision_id &&
        !d.retired_at &&
        [
          'builtin.regex',
          'builtin.pii',
          'builtin.secrets',
          'builtin.keywords'
        ].includes(d.type) &&
        (!policyProjectId || d.project_id === policyProjectId)
    )
  );

  let editingPolicy = $state<CapturePolicy | null>(null);
  let policyProjectId = $state('');
  let policyRoute = $state('');
  let policySinkId = $state('');
  let policyRatio = $state('1');
  let policyInclude = $state<string[]>(['input', 'output']);
  let policyRedactors = $state<string[]>([]);
  let redactorName = $state('');
  let refreshRedactors = $state(false);
  function addRedactor() {
    const name = redactorName.trim();
    if (
      name &&
      !policyRedactors.some(
        (value) => value.toLowerCase() === name.toLowerCase()
      ) &&
      policyRedactors.length < 128
    ) {
      policyRedactors = [...policyRedactors, name];
      redactorName = '';
    }
  }
  function moveRedactor(index: number, direction: number) {
    const next = [...policyRedactors];
    [next[index], next[index + direction]] = [
      next[index + direction],
      next[index]
    ];
    policyRedactors = next;
  }
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
    policyRedactors = [...policy.redact];
    refreshRedactors = false;
    redactorName = '';
    policyKeyIds = policy.key_ids.join(', ');
    policyEndUsers = policy.end_user_digests.join(', ');
    policyMaxBytes = String(policy.max_bytes);
    policyEnabled = policy.enabled;
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
  }

  function cancelPolicyEdit() {
    if (!policyBusy) resetPolicyForm();
  }

  const captureOwner = usePrincipalLifetime(() => {
    resetPolicyForm();
    policyBusy = false;
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

  function sinkOptionLabel(id: string) {
    const option = (sinkOptions.data ?? []).find(
      (s) => s.export_sink_id === id
    );
    return option ? `${option.name} (${option.type})` : id;
  }
</script>

{#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
{#if notice}<p class="section-help" role="status">{notice}</p>{/if}
<h3>Payload capture</h3>
<div class="capture-warning" role="note">
  Capture is opt-in and delivered from memory to an installation-owned sink.
  Policies without redactors capture <strong>unredacted</strong> content. Configured
  local detectors inspect selected fields before enqueue; failed or incomplete inspection
  drops the whole capture. Detector coverage is bounded and does not guarantee removal
  of every secret or personal detail.
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
        <fieldset class="form-field">
          <legend>Redactors in order</legend>
          <label for="policy-redactor">Local detector name</label>
          <input
            id="policy-redactor"
            list="capture-redactor-options"
            bind:value={redactorName}
            placeholder="Choose or enter a detector name"
          />
          <datalist id="capture-redactor-options">
            {#each eligibleRedactors as detector (detector.id)}
              <option value={detector.name}
                >{detector.type} · {detector.project_id}</option
              >
            {/each}
          </datalist>
          <button
            type="button"
            class="text-button"
            onclick={addRedactor}
            disabled={!redactorName.trim() || policyRedactors.length >= 128}
            >Add redactor</button
          >
          <ol>
            {#each policyRedactors as name, index (name)}
              <li>
                {name}
                <button
                  type="button"
                  aria-label={`Move ${name} up`}
                  disabled={index === 0}
                  onclick={() => moveRedactor(index, -1)}>↑</button
                >
                <button
                  type="button"
                  aria-label={`Move ${name} down`}
                  disabled={index === policyRedactors.length - 1}
                  onclick={() => moveRedactor(index, 1)}>↓</button
                >
                <button
                  type="button"
                  aria-label={`Remove ${name}`}
                  onclick={() =>
                    (policyRedactors = policyRedactors.filter(
                      (_, i) => i !== index
                    ))}>Remove</button
                >
              </li>
            {/each}
          </ol>
          <p class="section-help">
            Names resolve in the policy project, or the route's project when no
            project is selected. Empty means unredacted capture.
          </p>
          {#if editingPolicy}
            <p class="section-help">
              Other edits preserve pinned revisions. Changing this list adopts
              active revisions for every selected detector.
            </p>
            <button
              type="button"
              class="text-button"
              onclick={() => (refreshRedactors = true)}
              disabled={refreshRedactors || !policyRedactors.length}
              >Refresh pinned revisions on save</button
            >
            {#if refreshRedactors}<p role="status">
                Saving will adopt the active detector revisions.
              </p>{/if}
            <ul aria-label="Pinned redactor revisions">
              {#each editingPolicy.redactor_revisions ?? [] as pin (pin.revision_id)}
                <li>
                  {pin.name} · revision {pin.revision}
                  <span class="mono">{pin.revision_id}</span>
                </li>
              {/each}
            </ul>
          {/if}
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
    <button class="text-button" type="button" onclick={() => policies.refetch()}
      >Retry</button
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
          ><th scope="col">Includes</th><th scope="col">Redactors</th><th
            scope="col">Enabled</th
          ><th scope="col"><span class="sr-only">Actions</span></th></tr
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
            ><td>{policy.include.join(', ')}</td><td>
              {#if policy.redact.length}
                <ol>
                  {#each policy.redactor_revisions ?? [] as pin (pin.revision_id)}<li
                    >
                      {pin.name} · revision {pin.revision}
                      <span class="mono">{pin.revision_id}</span>
                    </li>{/each}
                </ol>
              {:else}Unredacted{/if}
            </td><td>{policy.enabled ? 'Yes' : 'No'}</td><td
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
