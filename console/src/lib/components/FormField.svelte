<script lang="ts">
  import type { Snippet } from 'svelte';

  type ControlAttributes = {
    id: string;
    'aria-invalid': 'true' | undefined;
    'aria-describedby': string | undefined;
  };

  let {
    label,
    error,
    full = false,
    control
  }: {
    label: string;
    error: string | undefined;
    full?: boolean;
    control: Snippet<[ControlAttributes]>;
  } = $props();

  const id = $props.id();
  const errorId = `${id}-error`;
</script>

<div class="form-field" class:full>
  <label for={id}>{label}</label>
  {@render control({
    id,
    'aria-invalid': error ? 'true' : undefined,
    'aria-describedby': error ? errorId : undefined
  })}
  {#if error}<p class="field-error" id={errorId}>{error}</p>{/if}
</div>
