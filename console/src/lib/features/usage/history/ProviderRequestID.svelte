<script lang="ts">
  import { copyText } from '#lib/clipboard.js';

  let { id }: { id: string } = $props();
  let copied = $state(false);
  let copyFailed = $state(false);

  async function copy() {
    const success = await copyText(id);
    copied = success;
    copyFailed = !success;
  }
</script>

<code>{id}</code>
<button
  type="button"
  class="text-button"
  aria-label={`Copy provider request ID ${id}`}
  onclick={copy}
>
  {copied ? 'Copied' : 'Copy'}
</button>
<span class="sr-only" role="status">
  {copied ? 'Provider request ID copied to clipboard.' : ''}
</span>
{#if copyFailed}
  <small role="alert">Clipboard unavailable. Copy the ID manually.</small>
{/if}

<style>
  .text-button {
    padding: 0.2rem 0.4rem;
  }

  small {
    display: block;
    color: var(--warning);
  }
</style>
