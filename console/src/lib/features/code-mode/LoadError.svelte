<script lang="ts">
  import type { Snippet } from 'svelte';
  import { errorMessage } from '$lib/api/http';

  let {
    error,
    onRetry,
    retryLabel = 'Retry',
    children
  }: {
    error: unknown;
    onRetry: () => void;
    retryLabel?: string;
    children?: Snippet;
  } = $props();
</script>

<p role="alert">
  {errorMessage(error)}
  <button class="text-button" type="button" onclick={onRetry}
    >{retryLabel}</button
  >
  {@render children?.()}
</p>
