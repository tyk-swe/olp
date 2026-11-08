<script lang="ts">
  import type { Snippet } from 'svelte';

  let {
    label,
    hint,
    error,
    full = false,
    children
  }: {
    label: string;
    hint?: string;
    error?: string;
    full?: boolean;
    children: Snippet<[{ id: string; describedBy: string | undefined }]>;
  } = $props();

  const id = $props.id();
  const describedBy = $derived(
    [hint && `${id}-hint`, error && `${id}-error`].filter(Boolean).join(' ') ||
      undefined
  );
</script>

<div class="form-field" class:full class:invalid={Boolean(error)}>
  <label for={id}>{label}</label>
  {@render children({ id, describedBy })}
  {#if hint}<small id="{id}-hint">{hint}</small>{/if}
  {#if error}<p id="{id}-error" class="inline-problem">{error}</p>{/if}
</div>
