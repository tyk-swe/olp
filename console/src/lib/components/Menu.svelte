<script lang="ts">
  import type { Snippet } from 'svelte';

  let {
    label,
    text = '',
    children
  }: { label: string; text?: string; children: Snippet } = $props();

  const id = $props.id();
  let trigger: HTMLButtonElement;
  let panel: HTMLDivElement;

  function place(event: ToggleEvent) {
    if (event.newState !== 'open') return;
    const anchor = trigger.getBoundingClientRect();
    const width = panel.offsetWidth;
    const left = Math.max(
      8,
      Math.min(anchor.right - width, innerWidth - width - 8)
    );
    const below = anchor.bottom + 4;
    const top =
      below + panel.offsetHeight > innerHeight
        ? Math.max(8, anchor.top - panel.offsetHeight - 4)
        : below;
    panel.style.left = `${left}px`;
    panel.style.top = `${top}px`;
  }

  function closeOnChoice(event: MouseEvent) {
    if (event.target instanceof Element && event.target.closest('a, button'))
      panel.hidePopover();
  }
</script>

<button
  bind:this={trigger}
  class="button button-secondary trigger"
  class:icon-only={!text}
  type="button"
  popovertarget="{id}-menu"
  aria-label={text ? undefined : label}
>
  {#if text}{text}{/if}<span class="dots" aria-hidden="true"
    >{text ? '▾' : '⋯'}</span
  >
</button>
<!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions (Clicks bubble from the menu's own buttons and links, which handle keyboard activation.) -->
<div
  bind:this={panel}
  id="{id}-menu"
  class="menu"
  popover="auto"
  aria-label={label}
  ontoggle={place}
  onclick={closeOnChoice}
>
  {@render children()}
</div>

<style>
  .trigger.icon-only {
    min-width: 2.75rem;
    padding-inline: 0.5rem;
  }

  .dots {
    color: var(--foreground-muted);
    font-size: 1rem;
    line-height: 1;
  }

  .menu {
    position: fixed;
    inset: auto;
    min-width: 13rem;
    margin: 0;
    padding: 0.35rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-card);
    background: var(--surface-raised);
    color: var(--foreground);
    box-shadow: var(--shadow-overlay);
  }

  .menu:popover-open {
    display: grid;
    animation: pop-in 180ms var(--ease-out);
  }

  .menu :global(:is(a, button)) {
    display: flex;
    min-height: 2.5rem;
    width: 100%;
    align-items: center;
    padding: 0.4rem 0.75rem;
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground-subtle);
    font-size: var(--text-body-sm);
    text-align: left;
    text-decoration: none;
    transition:
      background-color var(--motion),
      color var(--motion);
  }

  .menu :global(:is(a, button):hover:not(:disabled)) {
    background: var(--surface-hover);
    color: var(--foreground-hover);
  }

  .menu :global(:is(a, button).danger) {
    color: var(--danger);
  }

  .menu :global(:is(a, button):disabled) {
    opacity: 0.5;
  }

  .menu :global(hr) {
    margin: 0.3rem 0;
    border-top: 1px solid var(--border-hairline);
  }
</style>
