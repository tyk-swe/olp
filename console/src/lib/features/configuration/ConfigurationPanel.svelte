<script lang="ts">
  import { parseExternalReference } from '$lib/features/providers/externalReference';
  import { resolve } from '$app/paths';
  import { parseNativeJSON, stringifyNativeJSON } from '$lib/json/nativeJson';
  import { ApiProblem, errorMessage } from '$lib/api/http';
  import { copyText } from '$lib/clipboard';
  import { downloadBlob } from '$lib/download';
  import {
    applyConfiguration,
    exportConfiguration,
    grantEnrollments,
    missingSecretBindings,
    pinningProviders,
    planConfiguration,
    type ConfigurationDocument,
    type ConfigurationPlan,
    type ConfigurationPlanItem
  } from '$lib/features/configuration/api';

  let exportBusy = $state(false);
  let exportDigest = $state('');
  let exportedJSON = $state('');
  let copied = $state(false);
  let artifact = $state('');
  let artifactDocument = $state.raw<ConfigurationDocument | null>(null);
  let plan = $state<ConfigurationPlan | null>(null);
  let secrets = $state<Record<string, string>>({});
  let externalBindingsJSON = $state('');
  let applyResult = $state<'staged' | ''>('');
  let busy = $state(false);
  let error = $state('');

  async function loadExport() {
    if (exportBusy) return;
    exportBusy = true;
    error = '';
    try {
      const result = await exportConfiguration();
      exportDigest = result.digest;
      exportedJSON = stringifyNativeJSON(result.document, 2);
    } catch (problem) {
      error = errorMessage(problem);
    } finally {
      exportBusy = false;
    }
  }

  function downloadExport() {
    if (!exportedJSON) return;
    const blob = new Blob([exportedJSON], { type: 'application/json' });
    downloadBlob(blob, 'openllmproxy-configuration.json');
  }

  async function copyExport() {
    if (!exportedJSON) return;
    copied = await copyText(exportedJSON);
  }

  function parseArtifact() {
    error = '';
    applyResult = '';
    plan = null;
    // Secret bindings answer the artifact they were entered for. OLP refuses
    // one this artifact doesn't take, such as for a slot a grant now backs.
    secrets = {};
    externalBindingsJSON = '';
    try {
      artifactDocument = parseNativeJSON(artifact) as ConfigurationDocument;
    } catch {
      artifactDocument = null;
      plan = null;
      error = 'The artifact is not valid JSON.';
    }
  }

  async function upload(event: Event) {
    const file = (event.currentTarget as HTMLInputElement).files?.[0];
    if (!file) return;
    artifact = await file.text();
    parseArtifact();
  }

  function bindings(): Record<string, string> {
    return Object.fromEntries(
      Object.entries(secrets).filter(([, value]) => value !== '')
    );
  }

  function withBindings(
    operation: typeof planConfiguration,
    document: ConfigurationDocument
  ) {
    if (!externalBindingsJSON.trim()) return operation(document, bindings());
    const parsed: unknown = JSON.parse(externalBindingsJSON);
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed))
      throw new Error(
        'External bindings must be an object keyed by artifact credential reference.'
      );
    const references = Object.fromEntries(
      Object.entries(parsed).map(([name, value]) => [
        name,
        parseExternalReference(JSON.stringify(value))
      ])
    );
    return operation(document, bindings(), undefined, references);
  }

  async function runPlan() {
    if (busy || !artifactDocument) return;
    // A plan answers the document it reviewed; one that arrives after the
    // artifact changed must not enable Apply for the new document.
    const planned = artifactDocument;
    busy = true;
    error = '';
    applyResult = '';
    try {
      const result = await withBindings(planConfiguration, planned);
      if (artifactDocument === planned) plan = result;
    } catch (problem) {
      if (artifactDocument !== planned) return;
      plan = null;
      error = errorMessage(problem);
    } finally {
      busy = false;
    }
  }

  async function apply() {
    if (busy || !artifactDocument) return;
    const applied = artifactDocument;
    busy = true;
    error = '';
    applyResult = '';
    try {
      const result = await withBindings(applyConfiguration, applied);
      if (artifactDocument !== applied) return;
      plan = result;
      applyResult = 'staged';
    } catch (problem) {
      if (artifactDocument !== applied) return;
      if (problem instanceof ApiProblem && problem.problem.status === 409) {
        const replanned = await withBindings(planConfiguration, applied).catch(
          () => plan
        );
        if (artifactDocument === applied) plan = replanned;
      }
      error = errorMessage(problem);
    } finally {
      busy = false;
    }
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

{#snippet unavailablePlugin(item: ConfigurationPlanItem)}
  Plugin build <span class="mono">{item.key}</span> (pinned by {pinnedBy(
    item.key
  )})
  {#if item.detail === 'plugin_unconfined_disabled'}
    is unconfined, and this deployment does not enable unconfined plugins: an
    operator enables that tier, then plan again.
  {:else}
    {@const approval = item.detail === 'plugin_not_approved'}
    {approval ? 'awaits approval' : 'is not installed here'}: {approval
      ? 'an owner approves it'
      : 'install and approve it'} on the
    <a href={resolve('/plugins')}>Plugins page</a>, then plan again.
  {/if}
{/snippet}

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
      stage it on another installation. Providers and routes are staged as
      drafts — certification and activation stay local.
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
      <div class="promotion-actions">
        <button
          class="button button-secondary"
          onclick={runPlan}
          disabled={busy}>Plan</button
        >
      </div>
    {/if}
  </div>

  {#if plan}
    <div class="card" data-testid="promotion-plan">
      <small class="mono">Digest {plan.digest}</small>
      {#if actions.length}
        <h3>Actions</h3>
        <ul data-testid="plan-actions">
          {#each actions as item (item.kind + item.key + item.action)}<li>
              {itemLabel(item)}
            </li>{/each}
        </ul>
      {/if}
      {#if enrollments.length}
        <h3>Grant enrollment</h3>
        <p>
          Exports never carry grants. After applying, enroll a grant for each of
          these credential slots; their providers activate only once the slots
          they serve with hold one.
        </p>
        <ul data-testid="plan-grant-enrollments">
          {#each enrollments as ref (ref)}<li class="mono">{ref}</li>{/each}
        </ul>
      {/if}
      {#if plan.conflicts.length}
        <h3>Conflicts</h3>
        <ul data-testid="plan-conflicts" class="promotion-problems">
          {#each plan.conflicts as item (item.kind + item.key)}<li>
              {itemLabel(item)}
            </li>{/each}
        </ul>
      {/if}
      {#if plan.blockers.length}
        <h3>Blockers</h3>
        <ul data-testid="plan-blockers" class="promotion-problems">
          {#each plan.blockers as item (item.kind + item.key)}<li>
              {#if item.kind === 'plugin'}{@render unavailablePlugin(
                  item
                )}{:else}{itemLabel(item)}{/if}
            </li>{/each}
        </ul>
      {/if}
      {#each requiredSecrets as ref (ref)}
        <label for={`secret-${ref}`}
          >Credential <span class="mono">{ref}</span></label
        >
        <input
          id={`secret-${ref}`}
          type="password"
          autocomplete="off"
          bind:value={secrets[ref]}
        />
      {/each}
      {#if plan.blockers.length === 0 && plan.conflicts.length === 0}
        <button class="button" onclick={apply} disabled={busy}>
          {busy ? 'Applying…' : 'Apply'}
        </button>
      {/if}
      {#if applyResult === 'staged'}<p role="status" data-testid="apply-result">
          Configuration staged.
        </p>{/if}
    </div>
  {/if}
  {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
</section>
