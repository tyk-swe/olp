<script lang="ts">
  import type { PluginIndex } from './api';
  import { releaseStatus, sourceURL } from './pluginIndex';
  import { formatBytes, formatDate } from '$lib/format';

  let { index }: { index: PluginIndex } = $props();
</script>

<section class="card reviewed" aria-labelledby="plugin-index-heading">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Reviewed plugins</p>
      <h2 id="plugin-index-heading">Plugin index</h2>
      <p>
        Plugins reviewed for this release, signed with key {index.key_id} on
        {formatDate(index.published_at)}. Browsing installs nothing: build or
        download a module, upload it above, and OLP ties it to a reviewed
        release only when its digest matches. Approving its origins stays an
        explicit owner action.
      </p>
    </div>
  </div>
  <ul>
    {#each index.items as plugin (plugin.name)}
      <li>
        <div class="plugin-head">
          <strong>{plugin.name}</strong>
          <span>{plugin.description}</span>
          <small
            >Maintained by {plugin.maintainer} ·
            <a
              href={plugin.documentation_url}
              target="_blank"
              rel="noreferrer noopener">Documentation</a
            ></small
          >
        </div>
        {#each plugin.releases as release (release.digest)}
          <dl class="release">
            <div>
              <dt>Version</dt>
              <dd>
                {release.version}
                <span
                  class="badge"
                  class:success={release.approved}
                  class:warning={release.installed && !release.approved}
                  >{releaseStatus(release)}</span
                >
              </dd>
            </div>
            <div>
              <dt>Digest</dt>
              <dd><code class="mono">{release.digest}</code></dd>
            </div>
            <div>
              <dt>Origins</dt>
              <dd>{release.origins.join(', ')}</dd>
            </div>
            <div>
              <dt>Profiles</dt>
              <dd>{release.profiles.join(', ')}</dd>
            </div>
            <div>
              <dt>Module</dt>
              <dd>
                ABI {release.abi_version} · {formatBytes(release.size_bytes)} ·
                <a
                  href={sourceURL(plugin, release)}
                  target="_blank"
                  rel="noreferrer noopener"
                  >source at {release.commit.slice(0, 12)}</a
                >
              </dd>
            </div>
          </dl>
        {/each}
      </li>
    {/each}
  </ul>
</section>

<style>
  .reviewed {
    display: grid;
    gap: 1rem;
    margin-top: 1.5rem;
    padding: 1.5rem;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
  }
  .section-heading p {
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  ul {
    display: grid;
    gap: 1.25rem;
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .plugin-head {
    display: grid;
    gap: 0.2rem;
  }
  .plugin-head small {
    color: var(--foreground-muted);
  }
  .release {
    display: grid;
    gap: 0.3rem;
    margin: 0.6rem 0 0;
    font-size: var(--text-body-sm);
  }
  .release div {
    display: grid;
    grid-template-columns: 7rem 1fr;
    gap: 0.5rem;
  }
  dt {
    color: var(--foreground-muted);
  }
  dd {
    margin: 0;
    overflow-wrap: anywhere;
  }
</style>
