<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import { ProjectScopeField } from '#lib/features/access/index.js';
  import CaptureExpiryField from './CaptureExpiryField.svelte';
  import type {
    CapturePolicy,
    CaptureSinkOption
  } from '#lib/features/exports/api.js';
  import type { components } from '#lib/management/schema.js';

  type RedactorOption = components['schemas']['GuardrailResponse'];

  let {
    editingPolicy,
    busy,
    error,
    canManageInstallation,
    sinkOptions,
    eligibleRedactors,
    policyProjectId = $bindable(),
    policyRoute = $bindable(),
    policySinkId = $bindable(),
    policyRatio = $bindable(),
    policyInclude = $bindable(),
    policyRedactors = $bindable(),
    redactorName = $bindable(),
    policyKeyIds = $bindable(),
    policyEndUsers = $bindable(),
    policyMaxBytes = $bindable(),
    policyEnabled = $bindable(),
    policyExpiry = $bindable(),
    refreshRedactors = $bindable(),
    onClose,
    onSubmit,
    onCancel
  }: {
    editingPolicy: CapturePolicy | null;
    busy: boolean;
    error: string;
    canManageInstallation: boolean;
    sinkOptions: CaptureSinkOption[];
    eligibleRedactors: RedactorOption[];
    policyProjectId: string;
    policyRoute: string;
    policySinkId: string;
    policyRatio: string;
    policyInclude: string[];
    policyRedactors: string[];
    redactorName: string;
    policyKeyIds: string;
    policyEndUsers: string;
    policyMaxBytes: string;
    policyEnabled: boolean;
    policyExpiry: string | null | undefined;
    refreshRedactors: boolean;
    onClose: () => void;
    onSubmit: (event: SubmitEvent) => void;
    onCancel: () => void;
  } = $props();

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
  function toggleInclude(part: string) {
    policyInclude = policyInclude.includes(part)
      ? policyInclude.filter((value) => value !== part)
      : [...policyInclude, part];
  }
</script>

<Dialog
  title={editingPolicy ? 'Edit capture policy' : 'New capture policy'}
  description="Choose which prompts and responses are captured and how they are redacted."
  placement="side"
  {onClose}
>
  {#if error}<Callout tone="danger">{error}</Callout>{/if}
  <form class="create-form" onsubmit={onSubmit}>
    <fieldset class="submission-fields" disabled={busy}>
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
          >
            <option value="" disabled>
              Choose an installation sink
            </option>{#each sinkOptions as option (option.export_sink_id)}<option
                value={option.export_sink_id}
              >
                {option.name} ({option.type})
              </option>{/each}
          </select>
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
            <label class="inline-check">
              <input
                type="checkbox"
                checked={policyInclude.includes(part)}
                onchange={() => toggleInclude(part)}
              />{part}
            </label>
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
              <option value={detector.name}>
                {detector.type} · {detector.project_id}
              </option>
            {/each}
          </datalist>
          <button
            type="button"
            class="text-button"
            onclick={addRedactor}
            disabled={!redactorName.trim() || policyRedactors.length >= 128}
          >
            Add redactor
          </button>
          <ol>
            {#each policyRedactors as name, index (name)}
              <li>
                {name}
                <button
                  type="button"
                  aria-label={`Move ${name} up`}
                  disabled={index === 0}
                  onclick={() => moveRedactor(index, -1)}
                >
                  ↑
                </button>
                <button
                  type="button"
                  aria-label={`Move ${name} down`}
                  disabled={index === policyRedactors.length - 1}
                  onclick={() => moveRedactor(index, 1)}
                >
                  ↓
                </button>
                <button
                  type="button"
                  aria-label={`Remove ${name}`}
                  onclick={() =>
                    (policyRedactors = policyRedactors.filter(
                      (_, i) => i !== index
                    ))}
                >
                  Remove
                </button>
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
            >
              Refresh pinned revisions on save
            </button>
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
        <CaptureExpiryField
          current={editingPolicy?.expires_at}
          onChange={(expiry) => (policyExpiry = expiry)}
        />
        <div class="form-field">
          <label class="inline-check">
            <input type="checkbox" bind:checked={policyEnabled} />Enabled
          </label>
        </div>
      </div>
      <div class="form-actions">
        {#if editingPolicy}<button
            class="button button-secondary"
            type="button"
            onclick={onCancel}
          >
            Cancel
          </button>{/if}
        <button
          class="button button-primary"
          type="submit"
          disabled={busy ||
            !policySinkId ||
            !policyRatio.trim() ||
            !policyInclude.length}
        >
          {busy
            ? 'Saving…'
            : editingPolicy
              ? 'Save capture policy'
              : 'Create capture policy'}
        </button>
      </div>
    </fieldset>
  </form>
</Dialog>
