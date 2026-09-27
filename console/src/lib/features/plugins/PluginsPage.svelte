<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { formatBytes, formatDate } from '$lib/format';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { pluginKeys } from '$lib/features/plugins/pluginKeys';
  import {
    approvePlugin,
    installPlugin,
    listPlugins,
    pluginProblem,
    shortDigest,
    uninstallPlugin,
    type Plugin
  } from '$lib/features/plugins/api';

  const access = useRole();
  const canManage = $derived(access.can('plugins.manage'));

  const plugins = createQuery(() => ({
    queryKey: pluginKeys.list(),
    queryFn: ({ signal }) => listPlugins(signal)
  }));

  let module = $state<File | null>(null);
  let busy = $state('');
  let error = $state('');
  let notice = $state('');
  // The digest whose declared origins the owner is reviewing for approval.
  let reviewing = $state('');

  function title(plugin: Plugin) {
    return `${plugin.manifest.name} ${plugin.manifest.version}`;
  }

  async function run(label: string, action: () => Promise<void>) {
    busy = label;
    error = notice = '';
    try {
      await action();
    } catch (cause) {
      error = pluginProblem(cause) ?? errorMessage(cause);
    } finally {
      busy = '';
    }
  }

  async function install(event: SubmitEvent) {
    event.preventDefault();
    const form = event.currentTarget as HTMLFormElement;
    if (!module) {
      error = 'Choose a plugin module to upload.';
      return;
    }
    const upload = module;
    await run('install', async () => {
      const { plugin, created } = await installPlugin(upload);
      form.reset();
      module = null;
      await plugins.refetch();
      notice = created
        ? `Installed ${title(plugin)}. Review its origins and approve it before it can be used.`
        : `${title(plugin)} is already installed; nothing changed.`;
    });
  }

  async function approve(plugin: Plugin) {
    await run(`approve-${plugin.digest}`, async () => {
      await approvePlugin(plugin);
      reviewing = '';
      await plugins.refetch();
      notice = `Approved ${title(plugin)}. It may now reach its declared origins.`;
    });
  }

  async function uninstall(plugin: Plugin) {
    if (!confirm(`Uninstall ${title(plugin)} and delete its module?`)) return;
    await run(`uninstall-${plugin.digest}`, async () => {
      await uninstallPlugin(plugin);
      if (reviewing === plugin.digest) reviewing = '';
      await plugins.refetch();
      notice = `Uninstalled ${title(plugin)}.`;
    });
  }
</script>

<svelte:head><title>Plugins · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div>
    <p class="eyebrow">Gateway</p>
    <h1 class="page-title">Plugins</h1>
    <p class="page-description">
      Provider plugins supply authentication and hosting around built-in
      dialects. OpenLLMProxy runs them confined and stores each module by its
      digest. A plugin can't be used until an owner approves the origins it
      declares.
    </p>
  </div>
</div>

{#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
{#if notice}<div class="success-banner" role="status">{notice}</div>{/if}

{#if canManage}
  <section class="card install-panel" aria-labelledby="plugin-install-heading">
    <div>
      <p class="eyebrow">Install</p>
      <h2 id="plugin-install-heading">Upload a plugin module</h2>
      <p>
        OpenLLMProxy reads the manifest the module declares and holds the plugin
        until you approve its origins. Upstream terms of use are the operator's
        responsibility.
      </p>
    </div>
    <form onsubmit={install} novalidate>
      <label
        ><span>Plugin module (.wasm)</span><input
          type="file"
          accept=".wasm,application/wasm"
          onchange={(event) =>
            (module = event.currentTarget.files?.[0] ?? null)}
        /></label
      >
      <button
        class="button button-primary"
        type="submit"
        disabled={busy === 'install'}
        >{busy === 'install' ? 'Installing…' : 'Install plugin'}</button
      >
    </form>
  </section>
{:else}
  <ReadOnlyNote
    >Only owners can install, approve or uninstall plugins.</ReadOnlyNote
  >
{/if}

{#if plugins.isPending}
  <div class="loading-state" role="status">Loading plugins…</div>
{:else if plugins.isError}
  <div class="inline-problem" role="alert">
    {errorMessage(plugins.error)}
    <button class="text-button" type="button" onclick={() => plugins.refetch()}
      >Retry</button
    >
  </div>
{:else if !plugins.data?.length}
  <section class="card empty-state">
    <h2>No plugins installed</h2>
    <p>
      Installed plugins and the profiles and origins they declare appear here.
    </p>
  </section>
{:else}
  <div class="plugin-list">
    {#each plugins.data as plugin (plugin.digest)}
      {@const approved = Boolean(plugin.approved_at)}
      {@const headingId = `plugin-${plugin.digest}`}
      <article class="card plugin" aria-labelledby={headingId}>
        <header>
          <div>
            <h2 id={headingId}>{title(plugin)}</h2>
            {#if plugin.manifest.description}<p>
                {plugin.manifest.description}
              </p>{/if}
          </div>
          <span class="badge" class:success={approved} class:warning={!approved}
            >{approved ? 'Approved' : 'Pending approval'}</span
          >
        </header>
        <dl class="facts">
          <div>
            <dt>Digest</dt>
            <dd>
              <details>
                <summary
                  ><code>{shortDigest(plugin.digest)}</code><span
                    class="sr-only">Show the full digest</span
                  ></summary
                >
                <code class="digest">{plugin.digest}</code>
              </details>
            </dd>
          </div>
          <div>
            <dt>ABI</dt>
            <dd>{plugin.abi_version}</dd>
          </div>
          <div>
            <dt>Size</dt>
            <dd>{formatBytes(plugin.size_bytes)}</dd>
          </div>
          <div>
            <dt>Installed</dt>
            <dd>
              {formatDate(plugin.installed_at)}<br /><small
                >by {plugin.installed_by_email}</small
              >
            </dd>
          </div>
          {#if approved}
            <div>
              <dt>Approved</dt>
              <dd>
                {formatDate(plugin.approved_at)}<br /><small
                  >by {plugin.approved_by_email}</small
                >
              </dd>
            </div>
          {/if}
        </dl>
        <div class="declarations">
          <section aria-labelledby={`${headingId}-profiles`}>
            <h3 id={`${headingId}-profiles`}>Profiles</h3>
            <div class="table-shell">
              <table class="data-table">
                <thead
                  ><tr
                    ><th>Profile</th><th>Label</th><th>Dialect</th><th
                      >Address</th
                    ></tr
                  ></thead
                >
                <tbody>
                  {#each plugin.manifest.profiles as profile (profile.id)}
                    <tr
                      ><td><code>{profile.id}</code></td><td>{profile.label}</td
                      ><td><code>{profile.dialect}</code></td><td
                        ><code class="address">{profile.hosting.address}</code
                        ></td
                      ></tr
                    >
                  {/each}
                </tbody>
              </table>
            </div>
          </section>
          <section aria-labelledby={`${headingId}-origins`}>
            <h3 id={`${headingId}-origins`}>Declared origins</h3>
            {#if plugin.manifest.origins.length}
              <ul class="origins">
                {#each plugin.manifest.origins as origin (origin)}
                  <li><code>{origin}</code></li>
                {/each}
              </ul>
            {:else}
              <p class="muted">None. This plugin reaches no upstream origin.</p>
            {/if}
          </section>
        </div>
        {#if canManage}
          {#if reviewing === plugin.digest}
            <section class="approval" aria-labelledby={`${headingId}-approval`}>
              <h3 id={`${headingId}-approval`}>Approve these origins?</h3>
              <p>
                Once approved, {title(plugin)} may reach exactly
                {plugin.manifest.origins.length === 1
                  ? 'this origin'
                  : `these ${plugin.manifest.origins.length} origins`}. Its
                origins never change for this digest.
              </p>
              {#if plugin.manifest.origins.length}
                <ul class="origins">
                  {#each plugin.manifest.origins as origin (origin)}
                    <li><code>{origin}</code></li>
                  {/each}
                </ul>
              {/if}
              <div class="actions">
                <button
                  class="button button-primary"
                  type="button"
                  disabled={Boolean(busy)}
                  onclick={() => approve(plugin)}
                  >{busy === `approve-${plugin.digest}`
                    ? 'Approving…'
                    : 'Approve origins'}</button
                ><button
                  class="button button-secondary"
                  type="button"
                  onclick={() => (reviewing = '')}>Cancel</button
                >
              </div>
            </section>
          {/if}
          <div class="actions">
            {#if !approved && reviewing !== plugin.digest}<button
                class="button button-primary"
                type="button"
                disabled={Boolean(busy)}
                onclick={() => (reviewing = plugin.digest)}
                >Review and approve</button
              >{/if}
            <button
              class="button button-secondary danger-button"
              type="button"
              disabled={Boolean(busy)}
              onclick={() => uninstall(plugin)}
              >{busy === `uninstall-${plugin.digest}`
                ? 'Uninstalling…'
                : 'Uninstall'}</button
            >
          </div>
        {/if}
      </article>
    {/each}
  </div>
{/if}

<style>
  h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  h3 {
    margin: 0 0 0.5rem;
    font-size: var(--text-caption);
    font-weight: 500;
    color: var(--foreground-muted);
    text-transform: uppercase;
    letter-spacing: 0.06em;
  }
  .install-panel {
    display: flex;
    align-items: start;
    justify-content: space-between;
    gap: 2rem;
    margin-bottom: 1rem;
    padding: 1.5rem;
  }
  .install-panel p {
    margin: 0.4rem 0 0;
    color: var(--foreground-muted);
  }
  .install-panel form {
    display: grid;
    gap: 0.65rem;
  }
  .install-panel label {
    display: grid;
    gap: 0.4rem;
    font-weight: 500;
  }
  .install-panel input[type='file'] {
    max-width: 100%;
    color: var(--foreground);
    font-weight: 400;
  }
  .plugin-list {
    display: grid;
    gap: 1rem;
  }
  .plugin {
    display: grid;
    gap: 1.25rem;
    padding: 1.5rem;
    min-width: 0;
  }
  .plugin header {
    display: flex;
    align-items: start;
    justify-content: space-between;
    gap: 1rem;
  }
  .plugin header p {
    margin: 0.4rem 0 0;
    color: var(--foreground-muted);
  }
  .facts {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(10rem, 1fr));
    gap: 1rem;
    margin: 0;
  }
  .facts dt {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .facts dd {
    margin: 0.25rem 0 0;
    min-width: 0;
  }
  .facts small,
  .muted {
    color: var(--foreground-muted);
  }
  summary {
    cursor: pointer;
  }
  .digest {
    display: block;
    margin-top: 0.4rem;
    overflow-wrap: anywhere;
    font-size: var(--text-caption);
  }
  .declarations {
    display: grid;
    grid-template-columns: minmax(0, 2fr) minmax(0, 1fr);
    gap: 1.5rem;
  }
  .origins {
    display: grid;
    gap: 0.35rem;
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .origins code,
  .address {
    overflow-wrap: anywhere;
  }
  .approval {
    display: grid;
    gap: 0.75rem;
    padding: 1rem;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-control);
  }
  .approval p {
    margin: 0;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: 0.65rem;
  }
  .danger-button {
    color: var(--danger);
  }
  @media (max-width: 64rem) {
    .install-panel,
    .declarations {
      display: grid;
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
