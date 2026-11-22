<script lang="ts">
  import type { Project } from '#lib/features/access/index.js';
  import PublicationPromptFields from './PublicationPromptFields.svelte';
  import PublicationTools from './PublicationTools.svelte';
  import PublicationTrustFields from './PublicationTrustFields.svelte';
  import type {
    PublicationAction,
    PublicationForm,
    ServerPricingForm
  } from './form.js';
  import { listServers } from './api.js';
  import type { MCPServer, Publication, Revision } from './api.js';

  type Kind = 'agents' | 'prompts' | 'toolsets';

  let {
    kind,
    current,
    busy,
    canPrice,
    canManage,
    canTrust,
    unsaved,
    revisionID,
    inspected,
    projects,
    servers,
    form = $bindable(),
    server = $bindable(),
    serverID = $bindable(),
    pricing = $bindable(),
    credential = $bindable(),
    removeCredential = $bindable(),
    entraClientSecret = $bindable(),
    confirmRetire = $bindable(),
    onWrite,
    onAbandon,
    onChooseServer,
    onSavePricing,
    onAddTool,
    onInspect
  }: {
    kind: Kind;
    current: Publication | null;
    busy: boolean;
    canPrice: boolean;
    canManage: boolean;
    canTrust: boolean | undefined;
    unsaved: boolean;
    revisionID: string;
    inspected: Revision | null;
    projects: Project[];
    servers: Awaited<ReturnType<typeof listServers>>;
    form: PublicationForm;
    server: MCPServer | null;
    serverID: string;
    pricing: ServerPricingForm | null;
    credential: string;
    removeCredential: boolean;
    entraClientSecret: string;
    confirmRetire: boolean;
    onWrite: (action: PublicationAction) => void;
    onAbandon: () => void;
    onChooseServer: () => void;
    onSavePricing: () => void;
    onAddTool: (name: string) => void;
    onInspect: () => void;
  } = $props();
</script>

<section class="card stack-card" aria-label="Publication editor">
  <h2>{current ? 'Review publication' : 'New draft'}</h2>
  <form
    onsubmit={(event) => {
      event.preventDefault();
      void onWrite('save');
    }}
  >
    <fieldset disabled={busy || !canManage}>
      <label class="form-field"
        >Name <input bind:value={form.name} maxlength="100" required /></label
      ><label class="form-field"
        >Project <select
          aria-label="Project"
          bind:value={form.project}
          disabled={!!current}
          onchange={() => {
            server = null;
            pricing = null;
            serverID = '';
            form.tools = [];
          }}
          required
        >
          <option value="">Choose a project</option>

          {#each projects as project (project.id)}
            <option value={project.id}>{project.name}</option>
          {/each}
        </select>
      </label>

      {#if kind === 'toolsets'}
        <PublicationTools
          bind:form
          bind:server
          bind:serverID
          bind:pricing
          {servers}
          {busy}
          {canPrice}
          {onChooseServer}
          {onSavePricing}
          {onAddTool}
        />
      {:else if kind === 'prompts'}
        <PublicationPromptFields bind:form />
      {:else}
        <PublicationTrustFields
          bind:form
          {canTrust}
          bind:credential
          bind:removeCredential
          bind:entraClientSecret
        />
      {/if}<button class="button button-primary" type="submit"
        >Save draft</button
      >
    </fieldset>
  </form>
  {#if current}<p>
      Current revision: <code>{current.revision_id ?? 'Not published'}</code>
    </p>
    <button
      class="button button-secondary"
      type="button"
      disabled={busy ||
        !canManage ||
        unsaved ||
        !!credential ||
        !!entraClientSecret ||
        removeCredential}
      onclick={() => onWrite('publish')}>Publish saved draft</button
    >{#if unsaved}<p>Save edits before publishing.</p>{/if}
    <label class="form-field"
      >Revision UUID <input bind:value={revisionID} disabled={busy} /></label
    ><button
      class="button button-secondary"
      type="button"
      disabled={busy || !revisionID}
      onclick={onInspect}>Inspect immutable revision</button
    >
    {#if inspected}<p>
        Digest: <code>{inspected.digest}</code>; labels: {inspected.labels.join(
          ', '
        ) || 'none'}
      </p>
      <pre class="code-block">{JSON.stringify(
          inspected.definition,
          null,
          2
        )}</pre>{/if}
    {#if kind === 'prompts'}<button
        class="button button-secondary"
        type="button"
        disabled={busy || !canManage || !revisionID || unsaved}
        onclick={() => onWrite('staging')}>Set staging</button
      ><button
        class="button button-secondary"
        type="button"
        disabled={busy || !canManage || !revisionID || unsaved}
        onclick={() => onWrite('production')}>Set production</button
      >{/if}
    <label class="check-field"
      ><input
        type="checkbox"
        bind:checked={confirmRetire}
        disabled={busy || !canManage}
      /> Retire and revoke this publication</label
    ><button
      class="button button-secondary"
      type="button"
      disabled={busy || !canManage || !confirmRetire}
      onclick={() => onWrite('retire')}>Confirm retirement</button
    >
  {/if}<button
    class="button button-secondary"
    type="button"
    disabled={busy}
    onclick={onAbandon}>Close</button
  >
</section>

<style>
  form,
  fieldset {
    display: grid;
    gap: 1rem;
    min-width: 0;
    margin: 0;
    padding: 0;
  }
  fieldset > .button {
    justify-self: start;
  }
  .stack-card > .button + .button {
    margin-top: -0.5rem;
  }
</style>
