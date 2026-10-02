<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { copyText } from '$lib/clipboard';
  import { errorMessage } from '$lib/api/http';
  import type { CodeClientConfiguration, CodeRoute } from '$lib/api/code-mode';
  import { codeKeys } from './codeKeys';

  let {
    route,
    load
  }: {
    route: CodeRoute;
    load: (
      route: CodeRoute,
      signal?: AbortSignal
    ) => Promise<CodeClientConfiguration>;
  } = $props();
  let copied = $state('');
  const configuration = createQuery(() => ({
    queryKey: codeKeys.configuration(route.id, route.revision_id),
    queryFn: ({ signal }) => load(route, signal)
  }));
</script>

<section class="card" aria-label="Official client configuration">
  <h3>Official client configuration</h3>
  {#if configuration.isPending}<p role="status">
      Loading supported configuration…
    </p>
  {:else if configuration.isError}<p role="alert">
      {errorMessage(configuration.error)}
      <button
        type="button"
        class="text-button"
        onclick={() => configuration.refetch()}>Retry</button
      >
    </p>
  {:else if configuration.data}
    {@const config = configuration.data}
    <dl>
      <dt>Client release</dt>
      <dd>{config.client} · {config.client_version || 'Unqualified'}</dd>
      <dt>Route base URL</dt>
      <dd><code>{config.base_url}</code></dd>
      <dt>Native models</dt>
      <dd>{config.native_models.join(', ')}</dd>
    </dl>
    <p>
      Use your authorized OLP key. No upstream subscription credential belongs
      on the client machine.
    </p>
    {#if config.qualification_gaps.length}<h4>Qualification gaps</h4>
      <ul>
        {#each config.qualification_gaps as gap (gap)}<li>{gap}</li>{/each}
      </ul>{/if}
    <div class="form-field">
      <label for="code-client-configuration">Generated configuration</label>
      <textarea
        id="code-client-configuration"
        readonly
        value={config.configuration}
        rows="10"></textarea>
    </div>
    <button
      class="button button-secondary"
      type="button"
      onclick={async () =>
        (copied = (await copyText(config.configuration))
          ? 'Configuration copied.'
          : 'Clipboard unavailable; select and copy the configuration.')}
      >Copy configuration</button
    >
    <p role="status">{copied}</p>
  {/if}
</section>

<style>
  section {
    padding: 1.25rem;
    display: grid;
    gap: 0.75rem;
  }
  h3,
  h4,
  dt {
    font-weight: 600;
  }
  dl {
    display: grid;
    gap: 0.4rem;
  }
  dd,
  p,
  li {
    color: var(--foreground-subtle);
    overflow-wrap: anywhere;
    line-height: 1.6;
  }
  ul {
    list-style: disc;
    padding-left: 1.25rem;
  }
  textarea {
    overflow: auto;
    max-height: 30rem;
    background: var(--code-bg);
    padding: 1rem;
  }
</style>
