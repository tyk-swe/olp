<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { untrack } from 'svelte';
  import { copyText } from '$lib/clipboard';
  import { errorMessage } from '$lib/api/http';
  import SegmentedRadioGroup from '$lib/components/SegmentedRadioGroup.svelte';
  import type {
    CodeClient,
    CodeClientConfiguration,
    CodeClientSelection,
    CodeRoute
  } from '$lib/api/code-mode';
  import { codeKeys } from './codeKeys';
  import { adapterLabel, clientLabel } from './presentation';

  let {
    route,
    gatewayURL,
    load
  }: {
    route: CodeRoute;
    gatewayURL: string;
    load: (
      route: CodeRoute,
      selection: CodeClientSelection,
      signal?: AbortSignal
    ) => Promise<CodeClientConfiguration>;
  } = $props();
  let copied = $state('');
  // A selection belongs to the publication it was made for; a new
  // publication starts again from its defaults.
  let chosen = $state<{ revisionId: string; selection: CodeClientSelection }>();
  const selection = $derived<CodeClientSelection>(
    chosen?.revisionId === route.revision_id ? chosen.selection : {}
  );
  // Keep the pickers in place and focused while another selection loads or
  // after the server refuses one. Generated text uses only the current query.
  let shown = $state<{
    revisionId: string;
    config: CodeClientConfiguration;
  }>();
  const ids = $derived({
    client: `code-client-${route.id}`,
    model: `code-client-model-${route.id}`,
    smallModel: `code-client-small-model-${route.id}`,
    configuration: `code-client-configuration-${route.id}`
  });
  const configuration = createQuery(() => {
    const selectedRoute = route;
    const selected = selection;
    const loader = load;
    return {
      queryKey: codeKeys.configuration(
        selectedRoute.id,
        selectedRoute.revision_id,
        gatewayURL,
        selected
      ),
      queryFn: ({ signal }) =>
        untrack(async () => {
          const result = await loader(selectedRoute, selected, signal);
          shown = { revisionId: selectedRoute.revision_id, config: result };
          return result;
        })
    };
  });
  const config = $derived(configuration.data);
  const pickerConfig = $derived(
    config ??
      (shown?.revisionId === route.revision_id ? shown.config : undefined)
  );

  function select(change: CodeClientSelection) {
    copied = '';
    chosen = {
      revisionId: route.revision_id,
      selection: { ...selection, ...change }
    };
  }
</script>

<section class="card" aria-label="Client configuration">
  <h3>Client configuration</h3>
  {#if pickerConfig}
    <div class="selection" aria-busy={configuration.isFetching}>
      {#if pickerConfig.supported_clients.length > 1}
        <SegmentedRadioGroup
          label="Client"
          name={ids.client}
          value={pickerConfig.client}
          items={pickerConfig.supported_clients.map((client) => ({
            value: client,
            label: clientLabel(client)
          }))}
          onChange={(client) => select({ client: client as CodeClient })}
        />
      {/if}
      <div class="form-field">
        <label for={ids.model}>Client native model</label>
        <select
          id={ids.model}
          value={pickerConfig.model}
          onchange={(event) => select({ model: event.currentTarget.value })}
        >
          {#each pickerConfig.native_models as model (model)}<option
              value={model}>{model}</option
            >{/each}
        </select>
      </div>
      {#if pickerConfig.small_model != null}
        <div class="form-field">
          <label for={ids.smallModel}>Background model</label>
          <select
            id={ids.smallModel}
            aria-describedby="{ids.smallModel}-help"
            value={pickerConfig.small_model}
            onchange={(event) =>
              select({ small_model: event.currentTarget.value })}
          >
            {#each pickerConfig.native_models as model (model)}<option
                value={model}>{model}</option
              >{/each}
          </select>
          <small id="{ids.smallModel}-help"
            >{pickerConfig.client === 'claude-code'
              ? 'Haiku-class and other background requests.'
              : 'Session titles and other small tasks.'}</small
          >
        </div>
      {/if}
    </div>
  {/if}
  {#if configuration.isError}<p role="alert">
      {errorMessage(configuration.error)}
      <button
        type="button"
        class="text-button"
        onclick={() => configuration.refetch()}>Retry</button
      >
      {#if Object.keys(selection).length}<button
          type="button"
          class="text-button"
          onclick={() => (chosen = undefined)}>Use defaults</button
        >{/if}
    </p>
  {:else if !config}<p role="status">Loading supported configuration…</p>
  {:else}
    <dl>
      <dt>Subscription</dt>
      <dd><span class="badge">{adapterLabel(config.adapter)}</span></dd>
      <dt>Client release</dt>
      <dd>
        {clientLabel(config.client)}
        {config.client_version || 'Unqualified'}
      </dd>
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
      <label for={ids.configuration}
        >Generated configuration · {config.format.toUpperCase()}</label
      >
      <textarea
        id={ids.configuration}
        aria-describedby="{ids.configuration}-help"
        readonly
        value={config.configuration}
        rows="12"></textarea>
      <small id="{ids.configuration}-help"
        >{config.file
          ? `Save as ${config.file}.`
          : 'Source this file in your shell before starting the client.'}</small
      >
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
  .selection {
    display: grid;
    gap: 0.75rem;
  }
  dl {
    display: grid;
    gap: 0.4rem;
  }
  dd,
  p,
  li,
  small {
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
