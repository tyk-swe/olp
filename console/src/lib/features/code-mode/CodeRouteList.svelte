<script lang="ts">
  import RouteRevisions from './RouteRevisions.svelte';
  import ClientConfiguration from './ClientConfiguration.svelte';
  import { formatDate } from '#lib/format.js';
  import { adapterLabel } from './presentation';
  import type { CodeRoute } from '#lib/features/code-mode/api.js';

  let {
    items,
    allowed,
    busy,
    detail = $bindable(),
    gatewayURL,
    onEdit,
    onPublish
  }: {
    items: CodeRoute[];
    allowed: boolean;
    busy: boolean;
    detail: string;
    gatewayURL: string;
    onEdit: (route: CodeRoute) => void;
    onPublish: (route: CodeRoute) => void;
  } = $props();
</script>

<p>
  Route base paths select policy; the client sends native models unchanged. A
  pool may mix Codex, OpenCode Go and GLM Coding Plan accounts: each request
  goes to an account that lists its model and whose subscription serves the
  client's protocol. Connection edits require route republish. Pool/account/key
  permissions remain live.
</p>
{#each items as route (route.id)}
  <article class="card">
    <div class="actions">
      <h3>{route.slug}</h3>
      {#if allowed}<button
          class="button button-secondary"
          type="button"
          disabled={busy}
          onclick={() => onEdit(route)}>Edit draft</button
        ><button
          class="button button-primary"
          type="button"
          disabled={busy}
          onclick={() => onPublish(route)}>Publish route</button
        >{/if}<button
        class="button button-secondary"
        type="button"
        aria-expanded={detail === route.id}
        onclick={() => (detail = detail === route.id ? '' : route.id)}
        >Revisions and client setup</button
      >
    </div>
    <dl>
      <dt>Base path</dt>
      <dd><code>/code/{route.slug}</code></dd>
      <dt>Published subscriptions</dt>
      <dd class="badges">
        {#each route.adapters as adapter (adapter)}
          <span class="badge">{adapterLabel(adapter)}</span>
        {:else}
          {route.published_at
            ? 'No subscription adapter is published'
            : 'Set at publication'}
        {/each}
      </dd>
      <dt>Paid native models</dt>
      <dd>
        {Object.keys(route.paid_targets ?? {}).join(', ') || 'None'}
      </dd>
      <dt>Draft models</dt>
      <dd>{route.models.join(', ')}</dd>
      <dt>Draft pool</dt>
      <dd>{route.pool_id}</dd>
      <dt>Draft Codex CLI identity compatibility</dt>
      <dd>
        {route.codex_cli_identity
          ? 'Enabled, requires publication'
          : 'Disabled'}
      </dd>
      <dt>Draft enabled</dt>
      <dd>{route.enabled ? 'Yes' : 'No'}</dd>
      <dt>Latest publication</dt>
      <dd>
        {route.published_at
          ? `Revision ${route.revision} · ${formatDate(route.published_at)}`
          : 'Not published'}
      </dd>
    </dl>
    {#if detail === route.id}
      <RouteRevisions id={route.id} />
      {#if route.published_at}<ClientConfiguration {route} {gatewayURL} />
      {:else}<p>
          Publish this route before generating client configuration.
        </p>{/if}
    {/if}
  </article>
{/each}

<style>
  .actions {
    display: flex;
    gap: 0.65rem;
    align-items: center;
    flex-wrap: wrap;
  }
  h3 {
    flex: 1;
    font-weight: 600;
  }
  article {
    display: grid;
    gap: 1rem;
    padding: 1.25rem;
  }
  dl {
    display: grid;
    grid-template-columns: minmax(8rem, 1fr) 3fr;
    gap: 0.6rem 1rem;
  }
  dt {
    font-weight: 500;
  }
  .badges {
    display: flex;
    flex-wrap: wrap;
    gap: 0.4rem;
  }
  dd,
  p {
    color: var(--foreground-subtle);
    line-height: 1.6;
    overflow-wrap: anywhere;
  }
  @media (max-width: 40rem) {
    dl {
      grid-template-columns: 1fr;
    }
  }
</style>
