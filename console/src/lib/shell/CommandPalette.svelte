<script lang="ts">
  import { goto } from '$app/navigation';
  import { onMount } from 'svelte';
  import NavIcon from '#lib/shell/NavIcon.svelte';
  import type { PaletteEntry } from '#lib/shell/navigation.js';

  let {
    entries,
    onClose
  }: { entries: readonly PaletteEntry[]; onClose: () => void } = $props();

  const id = $props.id();
  let dialog: HTMLDialogElement;
  let query = $state('');
  let cursor = $state(0);

  const matches = $derived.by(() => {
    const words = query.toLowerCase().split(/\s+/).filter(Boolean);
    return entries.filter((entry) => {
      const haystack = `${entry.group} ${entry.label}`.toLowerCase();
      return words.every((word) => haystack.includes(word));
    });
  });
  const groups = $derived(
    [...new Set(matches.map((entry) => entry.group))].map((group) => ({
      group,
      entries: matches.filter((entry) => entry.group === group)
    }))
  );
  const selected = $derived(matches[Math.min(cursor, matches.length - 1)]);

  onMount(() => {
    const opener = document.activeElement;
    dialog.showModal();
    return () => {
      if (opener instanceof HTMLElement && opener.isConnected) opener.focus();
    };
  });

  function choose(entry: PaletteEntry | undefined) {
    if (!entry) return;
    dialog.close();
    // eslint-disable-next-line svelte/no-navigation-without-resolve -- navigation tree hrefs are built with resolve() in navigation.ts.
    void goto(entry.href);
  }

  function keydown(event: KeyboardEvent) {
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      const step = event.key === 'ArrowDown' ? 1 : -1;
      cursor = (cursor + step + matches.length) % Math.max(matches.length, 1);
      document
        .getElementById(`${id}-${cursor}`)
        ?.scrollIntoView({ block: 'nearest' });
      return;
    }
    if (event.key === 'Enter') {
      event.preventDefault();
      choose(selected);
    }
  }
</script>

<dialog
  class="palette"
  bind:this={dialog}
  aria-label="Search the console"
  onclose={onClose}
  onclick={(event) => {
    if (event.target === dialog) dialog.close();
  }}
>
  <div class="field">
    <NavIcon name="search" size={18} />
    <input
      type="text"
      placeholder="Search pages and actions"
      role="combobox"
      aria-expanded="true"
      aria-controls="{id}-list"
      aria-activedescendant={selected
        ? `${id}-${matches.indexOf(selected)}`
        : undefined}
      autocomplete="off"
      spellcheck="false"
      bind:value={query}
      oninput={() => (cursor = 0)}
      onkeydown={keydown}
    />
    <kbd>Esc</kbd>
  </div>
  <div class="results" id="{id}-list" role="listbox" aria-label="Results">
    {#each groups as { group, entries: items } (group)}
      <div role="group" aria-label={group}>
        <p class="group">{group}</p>
        {#each items as entry (entry.href + entry.label)}
          {@const index = matches.indexOf(entry)}
          <!-- svelte-ignore a11y_click_events_have_key_events (The combobox input owns keyboard selection through aria-activedescendant.) -->
          <div
            id="{id}-{index}"
            class="option"
            class:selected={entry === selected}
            role="option"
            tabindex="-1"
            aria-selected={entry === selected}
            onclick={() => choose(entry)}
            onpointermove={() => (cursor = index)}
          >
            <NavIcon
              name={entry.group === 'Create' ? 'plus' : entry.icon}
              size={16}
            />
            <span>{entry.label}</span>
          </div>
        {/each}
      </div>
    {:else}
      <p class="none">Nothing matches “{query}”</p>
    {/each}
  </div>
</dialog>

<style>
  .palette {
    width: min(36rem, calc(100vw - 2rem));
    max-height: min(32rem, calc(100dvh - 6rem));
    margin: 12dvh auto auto;
    padding: 0;
    overflow: hidden;
    border: 1px solid var(--border);
    border-radius: var(--radius-panel);
    background: var(--surface-raised);
    color: var(--foreground);
    box-shadow: var(--shadow-overlay);
  }
  .palette[open] {
    display: flex;
    flex-direction: column;
    animation: pop-in var(--dur-enter) var(--ease-out);
  }
  .palette::backdrop {
    background: var(--backdrop);
  }
  .field {
    display: flex;
    align-items: center;
    gap: 0.65rem;
    padding: 0 1rem;
    border-bottom: 1px solid var(--border-hairline);
    color: var(--foreground-muted);
  }
  input {
    min-height: 3.25rem;
    flex: 1;
    border: 0;
    background: transparent;
    color: var(--foreground);
    font-size: var(--text-body);
    outline: none;
    box-shadow: none;
  }
  kbd {
    padding: 0.05rem 0.35rem;
    border: 1px solid var(--border);
    border-radius: 4px;
    font-family: var(--font-mono);
    font-size: 0.68rem;
  }
  .results {
    overflow-y: auto;
    padding: 0.4rem;
  }
  .group {
    margin: 0.6rem 0 0.25rem;
    padding: 0 0.65rem;
    color: var(--foreground-muted);
    font-family: var(--font-mono);
    font-size: 0.68rem;
    letter-spacing: 0.08em;
    text-transform: uppercase;
  }
  .option {
    display: flex;
    min-height: 2.4rem;
    align-items: center;
    gap: 0.65rem;
    padding: 0 0.65rem;
    border-radius: var(--radius-control);
    color: var(--foreground-subtle);
    font-size: var(--text-body-sm);
    cursor: pointer;
  }
  .option.selected {
    background: var(--accent-soft);
    color: var(--foreground);
  }
  .none {
    padding: 1.5rem 0.65rem;
    color: var(--foreground-muted);
    text-align: center;
  }
</style>
