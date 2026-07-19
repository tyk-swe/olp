<script lang="ts">
  import { RadioGroup } from 'bits-ui';

  type Item = { value: string; label: string };
  let {
    label,
    name,
    value,
    items,
    onChange
  }: {
    label: string;
    name: string;
    value: string;
    items: Item[];
    onChange: (value: string) => void;
  } = $props();
</script>

<fieldset>
  <legend>{label}</legend>
  <RadioGroup.Root
    class="segmented-root"
    orientation="horizontal"
    {name}
    {value}
    onValueChange={onChange}
  >
    {#each items as item (item.value)}
      <RadioGroup.Item class="segmented-item" value={item.value} aria-label={item.label}>
        {#snippet children({ checked })}
          <span class:checked>{item.label}</span>
        {/snippet}
      </RadioGroup.Item>
    {/each}
  </RadioGroup.Root>
</fieldset>

<style>
  fieldset { min-width: 0; margin: 0; padding: 0; border: 0; }
  legend { margin-bottom: 0.4rem; font-weight: 700; }
  :global(.segmented-root) { display: flex; min-width: 0; gap: 0.4rem; }
  :global(.segmented-item) { min-height: 2.5rem; flex: 1; padding: 0; border: 1px solid var(--border); border-radius: 0.375rem; background: var(--surface); color: var(--foreground-muted); font-weight: 700; }
  :global(.segmented-item:hover) { background: var(--surface-hover); color: var(--foreground-hover); }
  :global(.segmented-item[data-state='checked']) { border-color: var(--accent); background: var(--accent-soft); color: var(--accent-strong); }
  span { display: flex; min-height: 2.4rem; align-items: center; justify-content: center; padding: 0.5rem; }
  @media (max-width: 38rem) { :global(.segmented-root) { display: grid; } span { justify-content: flex-start; } }
  @media (forced-colors: active) { :global(.segmented-item[data-state='checked']) { border: 2px solid Highlight; } }
</style>
