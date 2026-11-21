<script lang="ts">
  import PlanPreview from './PlanPreview.svelte';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { useMutation } from '#lib/editing/action.svelte.js';
  import { captureResult, readResult } from '#lib/editing/result.js';
  import { parseExternalReference } from '#lib/features/providers/index.js';
  import { parseNativeJSON, stringifyNativeJSON } from '#lib/nativeJson.js';
  import { ApiProblem, errorMessage } from '#lib/management/http.js';
  import { copyText } from '#lib/clipboard.js';
  import { downloadBlob } from '#lib/download.js';
  import {
    applyConfiguration,
    isCodeBindings,
    exportConfiguration,
    grantEnrollments,
    missingSecretBindings,
    pinningProviders,
    planConfiguration,
    type ConfigurationDocument,
    type ConfigurationPlan,
    type ConfigurationPlanItem
  } from '#lib/features/configuration/api.js';

  let exportBusy = $state(false);
  let exportDigest = $state('');
  let exportedJSON = $state('');
  let copied = $state(false);
  let artifact = $state('');
  let artifactDocument = $state.raw<ConfigurationDocument | null>(null);
  let plan = $state<ConfigurationPlan | null>(null);
  let secrets = $state<Record<string, string>>({});
  let externalBindingsJSON = $state('');
  let codeBindingsJSON = $state('');
  let applyResult = $state<'staged' | ''>('');
  const applyMutation = useMutation();
  let planBusy = $state(false);
  const busy = $derived(applyMutation.busy || planBusy);
  let error = $state('');
  let artifactRevision = 0;
  const captureOwner = usePrincipalLifetime(() => {
    retireArtifact();
    artifact = '';
    exportedJSON = exportDigest = '';
    copied = false;
    applyResult = '';
    exportBusy = false;
    planBusy = false;
    error = '';
  });

  async function loadExport() {
    if (exportBusy) return;
    const isCurrent = captureOwner();
    exportBusy = true;
    error = '';
    const result = await readResult(() => exportConfiguration());
    if (!isCurrent()) return;
    exportBusy = false;
    if (!result.ok) {
      error = errorMessage(result.error);
      return;
    }
    exportDigest = result.value.digest;
    exportedJSON = stringifyNativeJSON(result.value.document, 2);
  }

  function downloadExport() {
    if (!exportedJSON) return;
    const blob = new Blob([exportedJSON], { type: 'application/json' });
    downloadBlob(blob, 'openllmproxy-configuration.json');
  }

  async function copyExport() {
    if (!exportedJSON) return;
    const isCurrent = captureOwner();
    const result = await copyText(exportedJSON);
    if (isCurrent()) copied = result;
  }

  function retireArtifact() {
    artifactRevision += 1;
    error = '';
    applyResult = '';
    plan = null;
    // Secret bindings answer the artifact they were entered for. OLP refuses
    // one this artifact doesn't take, such as for a slot a grant now backs.
    applyMutation.reset();
    secrets = {};
    externalBindingsJSON = '';
    codeBindingsJSON = '';
    artifactDocument = null;
  }

  function parseArtifact() {
    retireArtifact();
    const parsed = captureResult(
      () => parseNativeJSON(artifact) as ConfigurationDocument
    );
    if (parsed.ok) artifactDocument = parsed.value;
    else {
      artifactDocument = null;
      plan = null;
      error = 'The artifact is not valid JSON.';
    }
  }

  async function upload(event: Event) {
    const file = (event.currentTarget as HTMLInputElement).files?.[0];
    if (!file) return;
    const isCurrent = captureOwner();
    retireArtifact();
    artifact = '';
    const revision = artifactRevision;
    const result = await readResult(() => file.text());
    if (!isCurrent() || artifactRevision !== revision) return;
    if (!result.ok) {
      error = errorMessage(result.error);
      return;
    }
    artifact = result.value;
    parseArtifact();
  }

  function bindings(): Record<string, string> {
    return Object.fromEntries(
      Object.entries(secrets).filter(([, value]) => value !== '')
    );
  }

  function bindingInputs() {
    const codeBindings: unknown = JSON.parse(codeBindingsJSON.trim() || '{}');
    if (!isCodeBindings(codeBindings))
      throw new Error(
        'Code bindings must map credentials, users and api_keys to destination IDs.'
      );
    const parsed: unknown = JSON.parse(externalBindingsJSON.trim() || '{}');
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed))
      throw new Error(
        'External bindings must be an object keyed by artifact credential reference.'
      );
    const externalBindings = Object.fromEntries(
      Object.entries(parsed).map(([name, value]) => [
        name,
        parseExternalReference(JSON.stringify(value))
      ])
    );
    return {
      secretBindings: bindings(),
      externalBindings,
      codeBindings
    };
  }

  function withBindings(
    operation: typeof planConfiguration,
    document: ConfigurationDocument
  ) {
    const inputs = bindingInputs();
    return operation(
      document,
      inputs.secretBindings,
      undefined,
      inputs.externalBindings,
      inputs.codeBindings
    );
  }

  async function runPlan() {
    if (busy || !artifactDocument) return;
    // A plan answers the document it reviewed; one that arrives after the
    // artifact changed must not enable Apply for the new document.
    const planned = artifactDocument;
    const isCurrent = captureOwner();
    planBusy = true;
    error = '';
    applyResult = '';
    const result = await readResult(() =>
      withBindings(planConfiguration, planned)
    );
    if (!isCurrent()) return;
    planBusy = false;
    if (artifactDocument !== planned) return;
    if (result.ok) plan = result.value;
    else {
      plan = null;
      error = errorMessage(result.error);
    }
  }

  async function apply() {
    if (busy || !artifactDocument) return;
    const applied = artifactDocument;
    const isCurrent = captureOwner();
    error = '';
    applyResult = '';
    const inputs = captureResult(() => bindingInputs());
    if (!inputs.ok) {
      error = errorMessage(inputs.error);
      return;
    }
    let failureCause: unknown;
    const committed = await applyMutation.submit(
      { document: applied, ...inputs.value },
      (submission, requestId) =>
        applyConfiguration(
          submission.document,
          submission.secretBindings,
          requestId,
          undefined,
          submission.externalBindings,
          submission.codeBindings
        ),
      (result, current) => {
        if (!current() || artifactDocument !== applied) return;
        plan = result;
        applyResult = 'staged';
      },
      {
        key: 'apply',
        failure: (problem) => {
          failureCause = problem;
          return errorMessage(problem);
        }
      }
    );
    if (committed || !isCurrent() || artifactDocument !== applied) return;
    if (
      failureCause instanceof ApiProblem &&
      failureCause.problem.status === 409
    ) {
      const replanned = await readResult(() =>
        withBindings(planConfiguration, applied)
      );
      if (!isCurrent() || artifactDocument !== applied) return;
      if (replanned.ok) plan = replanned.value;
    }
    error = errorMessage(failureCause);
  }

  const requiredSecrets = $derived(plan ? missingSecretBindings(plan) : []);
  const enrollments = $derived(plan ? grantEnrollments(plan) : []);
  const actions = $derived(
    plan?.actions.filter((item) => item.action !== 'enroll') ?? []
  );

  function pinnedBy(digest: string): string {
    return artifactDocument
      ? pinningProviders(artifactDocument, digest).join(', ')
      : '';
  }

  function itemLabel(item: ConfigurationPlanItem): string {
    return item.detail
      ? `${item.key} — ${item.action}: ${item.detail}`
      : `${item.key} — ${item.action}`;
  }
</script>

<section class="settings-section" aria-labelledby="promotion-title">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Configuration</p>
      <h2 id="promotion-title">Configuration promotion</h2>
    </div>
  </div>
  <div class="card">
    <p>
      Export the logical configuration as a secret-free artifact, then plan and
      stage it on another installation or project. Ordinary providers and routes
      are staged as drafts. Coding routes retain their draft and latest
      publication after explicit destination enrollment and binding.
    </p>
    <div class="promotion-actions">
      <button
        class="button button-secondary"
        onclick={loadExport}
        disabled={exportBusy}
      >
        {exportBusy ? 'Exporting…' : 'Export configuration'}
      </button>
      {#if exportedJSON}
        <button class="button button-secondary" onclick={downloadExport}
          >Download JSON</button
        >
        <button class="button button-secondary" onclick={copyExport}>
          {copied ? 'Copied' : 'Copy JSON'}
        </button>
      {/if}
    </div>
    {#if exportDigest}<small class="mono" data-testid="export-digest"
        >Digest {exportDigest}</small
      >{/if}
  </div>

  <div class="card">
    <label for="promotion-artifact">Artifact</label>
    <textarea
      id="promotion-artifact"
      rows="8"
      placeholder="Paste an exported artifact, or choose a file"
      bind:value={artifact}
      readonly={busy}
      oninput={parseArtifact}></textarea>
    <input
      aria-label="Upload artifact"
      type="file"
      accept="application/json,.json"
      disabled={busy}
      onchange={upload}
    />
    {#if artifactDocument}
      <label
        >External credential bindings<textarea
          class="filter-control"
          bind:value={externalBindingsJSON}
          oninput={() => {
            plan = null;
            applyResult = '';
          }}
          readonly={busy}
          rows="4"
          maxlength="65536"
          spellcheck="false"
          placeholder={'{"provider/primary":{"store":"gcp","secret_id":"projects/project/secrets/provider","version":"1"}}'}
        ></textarea></label
      >
      <p>
        Optionally bind artifact references to immutable secret-store versions.
        A reference must use either an external binding or a plaintext binding.
      </p>
      {#if artifactDocument.code}
        <label
          >Code destination bindings<textarea
            bind:value={codeBindingsJSON}
            oninput={() => {
              plan = null;
              applyResult = '';
            }}
            readonly={busy}
            rows="4"
            maxlength="65536"
            spellcheck="false"
            placeholder={'{"credentials":{"account-ref":"destination-credential-id"},"users":{},"api_keys":{}}'}
          ></textarea></label
        >
        <p>
          Enroll destination credentials first, then bind every credential,
          personal owner and API key reference to its destination ID. Rename
          route slugs when copying into another project on this installation.
        </p>
      {/if}
      <div class="promotion-actions">
        <button
          class="button button-secondary"
          onclick={runPlan}
          disabled={busy}>Plan</button
        >
      </div>
    {/if}
  </div>

  {#if plan}<PlanPreview
      {plan}
      {actions}
      {enrollments}
      {requiredSecrets}
      bind:secrets
      {busy}
      {applyResult}
      hasCode={Boolean(artifactDocument?.code)}
      {apply}
      {itemLabel}
      {pinnedBy}
    />{/if}
  {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
</section>
