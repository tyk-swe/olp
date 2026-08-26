<script lang="ts">
  import type { FieldIssue } from '$lib/api/http';

  // The API rejects a provider field with prose and a machine-readable code.
  // Both are shown: the prose explains, the code is what an operator quotes in
  // a bug report or looks up in the connector reference.
  let { issues }: { issues: FieldIssue[] } = $props();
</script>

{#if issues.length}
  <ul class="field-issues">
    {#each issues as issue (`${issue.field}:${issue.message}`)}
      <li>
        <strong>{issue.field.replaceAll('_', ' ')}</strong>
        {issue.message}{#if issue.code}<code>{issue.code}</code>{/if}
      </li>
    {/each}
  </ul>
{/if}

<style>
  .field-issues {
    margin: 0.5rem 0 0;
    padding-left: 1.1rem;
  }
  .field-issues li {
    margin-top: 0.2rem;
  }
  strong {
    text-transform: capitalize;
  }
  code {
    margin-left: 0.35rem;
    padding: 0.05rem 0.3rem;
    border-radius: 0.25rem;
    background: color-mix(in srgb, currentcolor 12%, transparent);
    font:
      0.7rem 'JetBrains Mono Variable',
      monospace;
  }
</style>
