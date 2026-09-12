<script lang="ts">
  import { resolve } from '$app/paths';
  import {
    createRouteDraft,
    activateRoute,
    type RouteDraftValidation
  } from '$lib/features/routes/api';
  import type { ProviderModelInventory } from '../models';
  import { errorMessage, isEtagMismatch } from '$lib/api/http';
  let {
    models,
    canManage
  }: { models: ProviderModelInventory[]; canManage: boolean } = $props();
  let chosen = $state<Array<{ entry: ProviderModelInventory; slug: string }>>(
    []
  );
  let busy = $state(false);
  let published = $state<string[]>([]);
  let errors = $state<string[]>([]);
  let created = $state<RouteDraftValidation[]>([]);
  function choose(entry: ProviderModelInventory, checked: boolean) {
    if (!checked) {
      chosen = chosen.filter((item) => item.entry.model.id !== entry.model.id);
      return;
    }
    const name = entry.metadata?.canonical_model || entry.model.upstream_model;
    const base = name
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, '-')
      .slice(0, entry.metadata?.canonical_model ? 63 : 54)
      .replace(/^-|-$/g, '');
    const slug = entry.metadata?.canonical_model
      ? base
      : `${base}-${entry.provider_id.slice(-8)}`;
    chosen = [...chosen, { entry, slug }];
  }
  async function create(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    errors = [];
    try {
      const groups = Map.groupBy(chosen, (item) => item.slug);
      for (const [slug, items] of groups) {
        if (created.some((item) => item.slug === slug)) continue;
        const operations = [
          ...new Set(
            items[0].entry.model.capabilities.map(
              (capability) => capability.operation
            )
          )
        ].filter((operation) =>
          items.every((item) =>
            item.entry.model.capabilities.some(
              (capability) =>
                capability.operation === operation &&
                capability.source === 'certified'
            )
          )
        );
        if (!operations.length) {
          errors.push(`${slug}: select models sharing a certified operation.`);
          continue;
        }
        try {
          const draft = await createRouteDraft({
            slug,
            operations,
            overall_timeout_ms: 120000,
            max_attempts: Math.min(items.length, 32767),
            targets: items.map(({ entry }) => ({
              provider_id: entry.provider_id,
              provider_model: entry.model.upstream_model,
              priority: 0,
              weight: 1,
              timeout_ms: 60000
            }))
          });
          created = [...created, draft];
        } catch (e) {
          errors.push(`${slug}: ${errorMessage(e)}`);
        }
      }
    } finally {
      busy = false;
    }
  }
  async function publish() {
    busy = true;
    errors = [];
    for (const route of created) {
      if (published.includes(route.id)) continue;
      try {
        await activateRoute(route);
        published = [...published, route.id];
      } catch (error) {
        errors.push(
          `${route.slug}: ${
            isEtagMismatch(error)
              ? 'This draft changed. Open it to review and publish the current version.'
              : errorMessage(error)
          }`
        );
      }
    }
    busy = false;
  }
</script>

{#if canManage}
  <details class="card bulk-routes">
    <summary>Compare models and create routes</summary>
    <p>
      Select activated models, then review route names. Assign the same route
      name to multiple models to create a route with multiple provider targets.
    </p>
    <div class="choices">
      {#each models as entry (entry.model.id)}<label
          ><input
            type="checkbox"
            checked={chosen.some(
              (item) => item.entry.model.id === entry.model.id
            )}
            disabled={busy || !entry.model.enabled || !entry.available}
            onchange={(event) => choose(entry, event.currentTarget.checked)}
          />{entry.provider_name} · {entry.model.display_name}<small
            >{entry.available ? 'Published' : 'Activate connection first'} · Context:
            {entry.metadata?.context_length ?? 'Unknown'} · Region: {entry
              .metadata?.region ?? 'Unknown'} · Output limit: {entry.metadata
              ?.max_output_tokens ?? 'Unknown'} · Quantization: {entry.metadata
              ?.quantization ?? 'Unknown'} · Zero retention: {entry.metadata
              ?.zero_data_retention == null
              ? 'Unknown'
              : entry.metadata.zero_data_retention
                ? 'Yes'
                : 'No'}</small
          ></label
        >{/each}
    </div>
    {#if chosen.length}<form onsubmit={create}>
        {#each chosen as item (item.entry.model.id)}<label class="route-name"
            >{item.entry.provider_name} · {item.entry.model
              .upstream_model}<input
              bind:value={item.slug}
              pattern="[a-z0-9]+(-[a-z0-9]+)*"
              maxlength="63"
              required
              aria-label="Route name for {item.entry.provider_name} {item.entry
                .model.upstream_model}"
            /></label
          >{/each}
        <button class="button button-primary" disabled={busy}
          >{busy ? 'Creating…' : 'Create reviewed route drafts'}</button
        >
      </form>{/if}
    {#if errors.length}<ul class="inline-problem" role="alert">
        {#each errors as error (error)}<li>{error}</li>{/each}
      </ul>{/if}
    {#if created.length}<p>
        Review the created drafts, then publish them. Each draft is validated
        during publication.
      </p>
      <ul>
        {#each created as route (route.id)}<li>
            <a href={resolve(`/routes/${route.id}`)}>{route.slug}</a>
            {published.includes(route.id) ? 'Published' : 'Draft'}
          </li>{/each}
      </ul>
      <button
        class="button button-primary"
        type="button"
        disabled={busy || published.length === created.length}
        onclick={publish}>Validate and publish reviewed routes</button
      >{/if}
  </details>
{/if}

<style>
  .bulk-routes {
    margin: 1rem 0;
    padding: 1.5rem;
  }
  summary {
    font-weight: 500;
    cursor: pointer;
  }
  p,
  small {
    color: var(--foreground-muted);
  }
  .choices {
    display: grid;
    gap: 0.5rem;
    max-height: 18rem;
    overflow: auto;
  }
  small {
    display: block;
    margin-left: 1.5rem;
  }
  .route-name {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 1rem;
    align-items: center;
    margin: 0.75rem 0;
  }
  @media (max-width: 42rem) {
    .route-name {
      grid-template-columns: 1fr;
    }
  }
</style>
