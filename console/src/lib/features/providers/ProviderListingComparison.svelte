<script lang="ts">
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { formatDate } from '#lib/format.js';
  import type { ProviderListingComparison } from '#lib/features/providers/models.js';
  import type { Provider } from '#lib/features/providers/providers.js';
  import type { ProviderEditor } from './providerEditor.svelte';

  let { current, editor }: { current: Provider; editor: ProviderEditor } =
    $props();
  const busy = $derived(editor.view.busy);
  const comparing = $derived(busy === 'listing-comparison');
  let result = $state<ProviderListingComparison>();
  usePrincipalLifetime(() => {
    result = undefined;
  });

  async function compare() {
    const comparison = await editor.compareListing();
    if (comparison) result = comparison;
  }
</script>

{#if current.state === 'active' && current.active_revision != null}
  <div class="listing-comparison">
    <div class="listing-heading">
      <div>
        <h3>Published listing comparison</h3>
        <p class="muted">
          Read the full upstream model listing with the published credential of
          revision {current.active_revision} and compare it with the published inventory.
          The comparison changes nothing.
        </p>
      </div>
      <button
        class="button button-secondary"
        type="button"
        onclick={compare}
        disabled={!editor.canManage || Boolean(busy)}
      >
        {comparing ? 'Comparing…' : 'Compare upstream listing'}
      </button>
    </div>
    {#if result}
      <div aria-live="polite">
        {#if result.status === 'incomplete'}
          <p class="warning" role="status">
            <strong>Comparison incomplete.</strong>
            {result.detail} No models are marked missing from an incomplete listing.
          </p>
        {:else}
          <p class="muted">
            {result.detail} Checked {formatDate(result.checked_at)} against revision
            {result.revision} with published credential version
            {result.credential_version}.
          </p>
          {#if result.new_models.length}
            <h4>New upstream models</h4>
            <ul>
              {#each result.new_models as model (model.upstream_model)}
                <li>
                  <code>{model.upstream_model}</code>
                  <span>{model.display_name}</span>
                </li>
              {/each}
            </ul>
          {/if}
          {#if result.changed_display_names.length}
            <h4>Changed display names</h4>
            <ul>
              {#each result.changed_display_names as model (model.upstream_model)}
                <li>
                  <code>{model.upstream_model}</code>
                  <span>
                    Published as {model.published_display_name}, listed as
                    {model.listed_display_name}
                  </span>
                </li>
              {/each}
            </ul>
          {/if}
          {#if result.not_listed.length}
            <h4>Not listed</h4>
            <p class="muted">
              These models were listed for this published credential before and
              are absent from the complete listing now. Not listed does not mean
              the model is retired.
            </p>
            <ul>
              {#each result.not_listed as model (model.upstream_model)}
                <li>
                  <code>{model.upstream_model}</code>
                  <span>{model.display_name}</span>
                  {#if model.routes.length}
                    <span class="routes">
                      Routes
                      {#each model.routes as route (route.id)}
                        <code>{route.slug}</code>
                      {/each}
                    </span>
                  {:else}
                    <span class="routes">No readable published routes</span>
                  {/if}
                </li>
              {/each}
            </ul>
          {/if}
          {#if result.unverified.length}
            <h4>Unverified omissions</h4>
            <p class="muted">
              These published models are absent, with no comparable complete
              listing evidence for this endpoint and credential version, so no
              missing verdict is made.
            </p>
            <ul>
              {#each result.unverified as model (model.upstream_model)}
                <li>
                  <code>{model.upstream_model}</code>
                  <span>{model.display_name}</span>
                  {#if model.routes.length}
                    <span class="routes">
                      Routes
                      {#each model.routes as route (route.id)}
                        <code>{route.slug}</code>
                      {/each}
                    </span>
                  {/if}
                </li>
              {/each}
            </ul>
          {/if}
          {#if !result.new_models.length && !result.changed_display_names.length && !result.not_listed.length && !result.unverified.length}
            <p class="muted">
              The complete listing matches the published inventory.
            </p>
          {/if}
        {/if}
      </div>
    {/if}
  </div>
{/if}

<style>
  .listing-comparison {
    margin-top: 1rem;
    padding-top: 1rem;
    border-top: 1px solid var(--border-hairline);
  }
  .listing-heading {
    display: flex;
    align-items: end;
    justify-content: space-between;
    gap: 1rem;
  }
  .muted,
  .routes {
    color: var(--foreground-muted);
  }
  .warning {
    color: var(--foreground-muted);
  }
  .routes {
    font-size: var(--text-body-sm);
  }
  h4 {
    margin-bottom: 0.4rem;
  }
  @media (max-width: 42rem) {
    .listing-heading {
      display: grid;
    }
  }
</style>
