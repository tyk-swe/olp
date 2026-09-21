<script lang="ts">
  import { ApiProblem, errorMessage } from '$lib/api/http';
  import { copyText } from '$lib/clipboard';
  import {
    applyConfiguration,
    exportConfiguration,
    missingSecretBindings,
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
  let artifactDocument = $state<ConfigurationDocument | null>(null);
  let plan = $state<ConfigurationPlan | null>(null);
  let secrets = $state<Record<string, string>>({});
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
      exportedJSON = JSON.stringify(result.document, null, 2);
    } catch (problem) {
      error = errorMessage(problem);
    } finally {
      exportBusy = false;
    }
  }

  function downloadExport() {
    if (!exportedJSON) return;
    const blob = new Blob([exportedJSON], { type: 'application/json' });
    const link = document.createElement('a');
    link.href = URL.createObjectURL(blob);
    link.download = 'openllmproxy-configuration.json';
    link.click();
    URL.revokeObjectURL(link.href);
  }

  async function copyExport() {
    if (!exportedJSON) return;
    copied = await copyText(exportedJSON);
  }

  function parseArtifact() {
    error = '';
    applyResult = '';
    try {
      artifactDocument = JSON.parse(artifact) as ConfigurationDocument;
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

  async function runPlan() {
    if (busy || !artifactDocument) return;
    busy = true;
    error = '';
    applyResult = '';
    try {
      plan = await planConfiguration(artifactDocument, bindings());
    } catch (problem) {
      plan = null;
      error = errorMessage(problem);
    } finally {
      busy = false;
    }
  }

  async function apply() {
    if (busy || !artifactDocument) return;
    busy = true;
    error = '';
    applyResult = '';
    try {
      plan = await applyConfiguration(artifactDocument, bindings());
      applyResult = 'staged';
    } catch (problem) {
      if (problem instanceof ApiProblem && problem.problem.status === 409) {
        plan = await planConfiguration(artifactDocument, bindings()).catch(
          () => plan
        );
      }
      error = errorMessage(problem);
    } finally {
      busy = false;
    }
  }

  const requiredSecrets = $derived(plan ? missingSecretBindings(plan) : []);

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
      oninput={parseArtifact}></textarea>
    <input
      aria-label="Upload artifact"
      type="file"
      accept="application/json,.json"
      onchange={upload}
    />
    {#if artifactDocument}
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
      {#if plan.actions.length}
        <h3>Actions</h3>
        <ul data-testid="plan-actions">
          {#each plan.actions as item (item.kind + item.key + item.action)}<li>
              {itemLabel(item)}
            </li>{/each}
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
              {itemLabel(item)}
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
