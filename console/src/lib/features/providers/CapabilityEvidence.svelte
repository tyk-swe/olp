<script lang="ts">
  import type { ProviderModel } from './models';
  import { formatDate } from '$lib/format';

  let { model }: { model: ProviderModel } = $props();

  function scope(source: string): string {
    switch (source) {
      case 'certified':
        return 'Tested server contract';
      case 'declared':
        return 'Declared by operator';
      case 'stale':
        return 'Stale contract test';
      default:
        return 'Unknown evidence';
    }
  }
</script>

<div class="capability-evidence" aria-label="Scoped capability evidence">
  <p>
    Model identity:
    {model.discovered_at
      ? `discovered ${formatDate(model.discovered_at)}`
      : 'manually declared or unknown'}. Discovery alone establishes no
    operation behavior.
  </p>
  {#if model.capabilities.length}
    <ul>
      {#each model.capabilities as capability (`${capability.operation}-${capability.surface}-${capability.mode}`)}
        <li>
          <code
            >{capability.operation}/{capability.surface}/{capability.mode}</code
          >
          · {scope(capability.source)}
          {#if capability.source === 'certified' && capability.certified_at}
            · <time datetime={capability.certified_at}
              >{formatDate(capability.certified_at)}</time
            >
          {/if}
        </li>
      {/each}
    </ul>
  {:else}
    <p>Operation evidence unknown; no reviewed tuple is present.</p>
  {/if}
  <p class="scope-note">
    Each record applies only to its named operation, surface and mode. Native
    execution and translated interaction qualification are separate. No
    empirical model quality comparison is recorded here.
  </p>
</div>

<style>
  .capability-evidence {
    display: grid;
    gap: 0.4rem;
    padding: 0.7rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    font-size: var(--text-caption);
  }
  p,
  ul {
    margin: 0;
  }
  ul {
    padding-left: 1.2rem;
  }
  li {
    padding: 0.15rem 0;
  }
  .scope-note {
    color: var(--foreground-muted);
  }
  code {
    overflow-wrap: anywhere;
  }
</style>
