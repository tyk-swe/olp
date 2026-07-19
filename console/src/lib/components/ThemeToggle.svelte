<script lang="ts">
  import { onMount } from 'svelte';
  import NavIcon from './NavIcon.svelte';

  type Theme = 'light' | 'dark';
  const storageKey = 'olp.console.theme';
  let theme = $state<Theme>('light');
  let ready = $state(false);

  function preferredTheme(): Theme {
    const saved = window.localStorage.getItem(storageKey);
    if (saved === 'light' || saved === 'dark') return saved;
    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }

  function apply(next: Theme) {
    theme = next;
    document.documentElement.dataset.theme = next;
  }

  function toggle() {
    const next = theme === 'dark' ? 'light' : 'dark';
    apply(next);
    window.localStorage.setItem(storageKey, next);
  }

  onMount(() => {
    apply(preferredTheme());
    ready = true;
  });
</script>

<button
  class="icon-button"
  type="button"
  aria-label={ready && theme === 'dark' ? 'Use light theme' : 'Use dark theme'}
  title={ready && theme === 'dark' ? 'Use light theme' : 'Use dark theme'}
  onclick={toggle}
>
  <NavIcon name={ready && theme === 'dark' ? 'sun' : 'moon'} />
</button>

<style>
  .icon-button {
    display: inline-grid;
    width: 2.5rem;
    height: 2.5rem;
    place-items: center;
    border: 1px solid transparent;
    border-radius: 0.375rem;
    background: transparent;
    color: var(--foreground-muted);
  }

  .icon-button:hover {
    border-color: var(--border);
    background: var(--surface-hover);
    color: var(--foreground-hover);
  }
</style>
