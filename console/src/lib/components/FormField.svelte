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
    control
  }: {
    label: string;
    error: string | undefined;
    control: Snippet<[ControlAttributes]>;
  } = $props();

  const id = $props.id();
  const errorId = `${id}-error`;
</script>

<div class="form-field">
  <label for={id}>{label}</label>
  {@render control({
    id,
    'aria-invalid': error ? 'true' : undefined,
    'aria-describedby': error ? errorId : undefined
  })}
  {#if error}<p class="field-error" id={errorId}>{error}</p>{/if}
</div>
