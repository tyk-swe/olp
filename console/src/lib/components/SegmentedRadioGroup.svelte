<script lang="ts">
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
  <div class="segmented-root">
    {#each items as item (item.value)}
      <label class="segmented-item">
        <input
          type="radio"
          {name}
          value={item.value}
          checked={value === item.value}
          onchange={() => onChange(item.value)}
        />
        <span>{item.label}</span>
      </label>
    {/each}
  </div>
</fieldset>

<style>
  fieldset {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  legend {
    margin-bottom: 0.4rem;
    font-weight: 700;
  }
  .segmented-root {
    display: flex;
    min-width: 0;
    gap: 0.4rem;
  }
  .segmented-item {
    position: relative;
    min-height: 2.5rem;
    flex: 1;
    cursor: pointer;
  }
  input {
    position: absolute;
    width: 1px;
    height: 1px;
    opacity: 0;
  }
  span {
    display: flex;
    min-height: 2.5rem;
    align-items: center;
    justify-content: center;
    padding: 0.5rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
    background: var(--surface);
    color: var(--foreground-muted);
    font-weight: 700;
  }
  .segmented-item:hover span {
    background: var(--surface-hover);
    color: var(--foreground-hover);
  }
  input:focus-visible + span {
    outline: 2px solid var(--accent);
    outline-offset: 2px;
  }
  input:checked + span {
    border-color: var(--accent);
    background: var(--accent-soft);
    color: var(--accent-strong);
  }
  @media (max-width: 38rem) {
    .segmented-root {
      display: grid;
    }
    span {
      justify-content: flex-start;
    }
  }
  @media (forced-colors: active) {
    input:checked + span {
      border: 2px solid Highlight;
    }
  }
</style>
