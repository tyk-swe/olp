<script lang="ts">
  import type { FieldIssue } from '$lib/api/http';

  // The API rejects a provider field with prose and a machine-readable code.
  // Both are shown: the prose explains, the code is what an operator quotes in
  // a bug report or looks up in the connector reference.
  //
  // `error_codes[field][i]` always pairs with `errors[field][i]`, so the API
  // pads an uncoded message with an empty string. The truthiness check below
  // reads that placeholder as "no code" and leaves the chip off.
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
    /* The global reset strips list markers; the indent only reads as a list
       once they are back. */
    list-style: disc;
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
