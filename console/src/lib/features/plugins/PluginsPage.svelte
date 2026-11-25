<script lang="ts">
  import PluginCard from './PluginCard.svelte';
  import { useAction } from '#lib/editing/action.svelte.js';
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage, isEtagMismatch } from '#lib/management/http.js';

  import ReadOnlyNote from '#lib/components/ReadOnlyNote.svelte';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { pluginKeys } from '#lib/features/plugins/pluginKeys.js';
  import UnconfinedPlugins from '#lib/features/plugins/UnconfinedPlugins.svelte';
  import MCPExecutables from '#lib/features/plugins/MCPExecutables.svelte';
  import PluginIndexSection from '#lib/features/plugins/PluginIndex.svelte';
  import { indexEntryForDigest } from '#lib/features/plugins/pluginIndex.js';
  import {
    approvePlugin,
    getPluginIndex,
    installPlugin,
    listPlugins,
    pluginProblem,
    pluginTitle,
    uninstallPlugin,
    type Plugin
  } from '#lib/features/plugins/api.js';
  import { providerKeys } from '#lib/features/providers/index.js';

  const access = useRole();
  const canManage = $derived(access.can('plugins.manage'));
  const queryClient = useQueryClient();

  const pluginIndex = createQuery(() => ({
    queryKey: pluginKeys.index(),
    queryFn: ({ signal }) => getPluginIndex(signal)
  }));
  const plugins = createQuery(() => ({
    queryKey: pluginKeys.list(),
    queryFn: ({ signal }) => listPlugins(signal)
  }));

  let module = $state<File | null>(null);
  const pluginAction = useAction();
  let error = $state('');
  let notice = $state('');
  // The digest whose declared origins the owner is reviewing for approval.
  let reviewing = $state('');

  async function refreshPlugins() {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: pluginKeys.root }),
      queryClient.invalidateQueries({ queryKey: providerKeys.profiles() })
    ]);
  }

  /** Runs an owner's action, then refreshes the profile catalogue, plugins
   * and unconfined executables, even once OLP refused it: another owner may
   * have changed the plugin meanwhile, and trying again needs its ETag. */
  async function run(label: string, action: () => Promise<void>) {
    error = notice = '';
    await pluginAction.run(action, undefined, {
      key: label,
      failure: (cause) =>
        isEtagMismatch(cause)
          ? 'This plugin changed meanwhile. Review it as it is now, then try again.'
          : pluginProblem(cause)
    });
    error = pluginAction.error;
    await refreshPlugins();
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
      notice = created
        ? `Installed ${pluginTitle(plugin)}. Review its origins and approve it before it can be used.`
        : `${pluginTitle(plugin)} is already installed; nothing changed.`;
    });
  }

  async function approve(plugin: Plugin) {
    await run(`approve-${plugin.digest}`, async () => {
      await approvePlugin(plugin);
      reviewing = '';
      notice = `Approved ${pluginTitle(plugin)}. It may now reach its declared origins.`;
    });
  }

  async function uninstall(plugin: Plugin) {
    if (
      !(await confirmAction({
        title: `Uninstall ${pluginTitle(plugin)}?`,
        body: plugin.executable
          ? 'Its permission is withdrawn. The executable stays in the image.'
          : 'Its module is deleted.',
        confirmLabel: 'Uninstall',
        danger: true
      }))
    )
      return;
    await run(`uninstall-${plugin.digest}`, async () => {
      await uninstallPlugin(plugin);
      if (reviewing === plugin.digest) reviewing = '';
      notice = `Uninstalled ${pluginTitle(plugin)}.`;
    });
  }
</script>

<PageTitle title="Plugins" />

<PageHeader
  title="Plugins"
  description="Provider plugins supply authentication and hosting around built-in dialects. OpenLLMProxy runs them confined and stores each module by its digest. A plugin can't be used until an owner approves the origins it declares. A deployment may also enable experimental unconfined plugins, which an owner permits one by one."
/>
{#if error}<div class="inline-problem" role="alert">{error}</div>{/if}
{#if notice}<Callout tone="success">{notice}</Callout>{/if}

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
      <label>
        <span>Plugin module (.wasm)</span><input
          type="file"
          accept=".wasm,application/wasm"
          onchange={(event) =>
            (module = event.currentTarget.files?.[0] ?? null)}
        />
      </label>
      <button
        class="button button-primary"
        type="submit"
        disabled={pluginAction.key === 'install'}
      >
        {pluginAction.key === 'install' ? 'Installing…' : 'Install plugin'}
      </button>
    </form>
  </section>
{:else}
  <ReadOnlyNote>
    Only owners can install, approve or uninstall plugins.
  </ReadOnlyNote>
{/if}

{#if plugins.isPending}
  <div class="loading-state" role="status">Loading plugins…</div>
{:else if plugins.isError}
  <div class="inline-problem" role="alert">
    {errorMessage(plugins.error)}
    <button class="text-button" type="button" onclick={() => plugins.refetch()}>
      Retry
    </button>
  </div>
{:else if !plugins.data?.items.length}
  <section class="card empty-state">
    <h2>No plugins installed</h2>
    <p>
      Installed plugins and the profiles and origins they declare appear here.
    </p>
  </section>
{:else}
  <div class="plugin-list">
    {#each plugins.data.items as plugin (plugin.digest)}
      <PluginCard
        {plugin}
        listed={indexEntryForDigest(pluginIndex.data, plugin.digest)}
        {canManage}
        bind:reviewing
        actionKey={pluginAction.key}
        {approve}
        {uninstall}
      />
    {/each}
  </div>
{/if}

{#if pluginIndex.data}<PluginIndexSection index={pluginIndex.data} />{/if}

{#if plugins.data}
  <UnconfinedPlugins
    enabled={plugins.data.unconfined_plugins_enabled}
    {canManage}
    onPermitted={refreshPlugins}
  />
  <MCPExecutables
    enabled={plugins.data.unconfined_plugins_enabled}
    {canManage}
  />
{/if}

<style>
  h2 {
    margin: 0;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
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
</style>
