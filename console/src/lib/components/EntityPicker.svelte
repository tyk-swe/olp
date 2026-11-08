<script lang="ts">
  import { entityOptions, type EntityKind } from './entityOptions.svelte.js';

  let {
    id,
    kind,
    value = $bindable(''),
    emptyLabel,
    disabled = false,
    required = false,
    onchange
  }: {
    id: string;
    kind: EntityKind;
    value?: string;
    emptyLabel: string;
    disabled?: boolean;
    required?: boolean;
    onchange?: (value: string) => void;
  } = $props();

  // svelte-ignore state_referenced_locally (an entity picker never changes kind after mount)
  const options = entityOptions(kind);
  const known = $derived(
    options.data?.some((option) => option.value === value) ?? false
  );
</script>

{#if options.isError || (!options.isEnabled && !options.data)}
  <input
    {id}
    class="mono"
    bind:value
    {disabled}
    {required}
    placeholder="Paste an ID"
    autocomplete="off"
    spellcheck="false"
    onchange={(event) => onchange?.(event.currentTarget.value)}
  />
{:else}
  <select
    {id}
    bind:value
    disabled={disabled || options.isPending}
    {required}
    aria-busy={options.isPending}
    onchange={(event) => onchange?.(event.currentTarget.value)}
  >
    <option value="">{options.isPending ? 'Loading…' : emptyLabel}</option>
    {#if value && !known}<option {value}>{value}</option>{/if}
    {#each options.data ?? [] as option (option.value)}
      <option value={option.value}
        >{option.detail
          ? `${option.label} · ${option.detail}`
          : option.label}</option
      >
    {/each}
  </select>
{/if}
