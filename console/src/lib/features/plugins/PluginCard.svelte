<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { formatBytes, formatDate } from '#lib/format.js';
  import { indexEntryForDigest } from './pluginIndex.js';
  import { pluginTitle, shortDigest, type Plugin } from './api';

  let {
    plugin,
    listed,
    canManage,
    reviewing = $bindable(),
    actionKey,
    approve,
    uninstall
  }: {
    plugin: Plugin;
    listed: ReturnType<typeof indexEntryForDigest>;
    canManage: boolean;
    reviewing: string;
    actionKey: string;
    approve: (plugin: Plugin) => void;
    uninstall: (plugin: Plugin) => void;
  } = $props();
  const approved = $derived(Boolean(plugin.approved_at));
  const headingId = $derived(`plugin-${plugin.digest}`);
  const status = $derived(plugin.approved_at ? 'Approved' : 'Pending approval');
</script>

<article class="card plugin" aria-labelledby={headingId}>
  <header>
    <div>
      <h2 id={headingId}>{pluginTitle(plugin)}</h2>
      {#if plugin.manifest.description}<p>
          {plugin.manifest.description}
        </p>{/if}
      <p class="listing">
        {listed
          ? `Listed in the reviewed plugin index as ${listed.plugin.name} ${listed.release.version}.`
          : 'Not listed in the reviewed plugin index: review its source and origins yourself before approving.'}
      </p>
    </div>
    <div class="badges">
      {#if plugin.executable}<span class="badge danger">Unconfined</span>{/if}
      <span class="badge" class:success={approved} class:warning={!approved}
        >{status}</span
      >
    </div>
  </header>
  <dl class="facts">
    <div>
      <dt>Digest</dt>
      <dd>
        <details>
          <summary
            ><code>{shortDigest(plugin.digest)}</code><span class="sr-only"
              >Show the full digest</span
            ></summary
          >
          <code class="digest">{plugin.digest}</code>
        </details>
      </dd>
    </div>
    {#if plugin.executable}
      <div>
        <dt>Executable</dt>
        <dd><code>{plugin.executable}</code></dd>
      </div>
    {/if}
    <div>
      <dt>ABI</dt>
      <dd>{plugin.abi_version}</dd>
    </div>
    <div>
      <dt>Size</dt>
      <dd>{formatBytes(plugin.size_bytes)}</dd>
    </div>
    <div>
      <dt>{plugin.executable ? 'Permitted' : 'Installed'}</dt>
      <dd>
        {formatDate(plugin.installed_at)}<br /><small
          >by {plugin.installed_by_email}</small
        >
      </dd>
    </div>
    {#if approved && !plugin.executable}
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
      <ScrollTable label="Plugin profiles">
        <table class="data-table">
          <thead
            ><tr
              ><th>Profile</th><th>Label</th><th>Dialect</th><th
                >Authentication</th
              ><th>Address</th></tr
            ></thead
          >
          <tbody>
            {#each plugin.manifest.profiles as profile (profile.id)}
              <tr
                ><td><code>{profile.id}</code></td><td>{profile.label}</td><td
                  ><code>{profile.dialect}</code></td
                ><td
                  >{profile.grant
                    ? 'Grant, enrolled by the plugin'
                    : 'Static credential'}</td
                ><td><code class="address">{profile.hosting.address}</code></td
                ></tr
              >
            {/each}
          </tbody>
        </table>
      </ScrollTable>
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
    {#if reviewing === plugin.digest && !approved}
      <section class="approval" aria-labelledby={`${headingId}-approval`}>
        <h3 id={`${headingId}-approval`}>Approve these origins?</h3>
        <p>
          Once approved, {pluginTitle(plugin)} may reach exactly
          {plugin.manifest.origins.length === 1
            ? 'this origin'
            : `these ${plugin.manifest.origins.length} origins`}. Its origins
          never change for this digest.
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
            disabled={Boolean(actionKey)}
            onclick={() => approve(plugin)}
            >{actionKey === `approve-${plugin.digest}`
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
          disabled={Boolean(actionKey)}
          onclick={() => (reviewing = plugin.digest)}>Review and approve</button
        >{/if}
      <button
        class="button button-secondary button-danger"
        type="button"
        disabled={Boolean(actionKey)}
        onclick={() => uninstall(plugin)}
        >{actionKey === `uninstall-${plugin.digest}`
          ? 'Uninstalling…'
          : 'Uninstall'}</button
      >
    </div>
  {/if}
</article>

<style>
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
  .badges {
    display: flex;
    flex-wrap: wrap;
    justify-content: flex-end;
    gap: 0.4rem;
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
  .facts small {
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
  @media (max-width: 64rem) {
    .declarations {
      display: grid;
      grid-template-columns: minmax(0, 1fr);
    }
  }
  @media (max-width: 64rem) {
    .declarations {
      display: grid;
      grid-template-columns: minmax(0, 1fr);
    }
  }
  .listing {
    margin: 0.3rem 0 0;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
</style>
