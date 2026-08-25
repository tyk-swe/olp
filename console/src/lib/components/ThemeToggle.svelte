<script lang="ts">
  import { onMount } from 'svelte';
  import NavIcon from './NavIcon.svelte';

  type Theme = 'light' | 'dark';
  const storageKey = 'olp.console.theme';
  let theme = $state<Theme>('light');
  let ready = $state(false);
  // Blocked storage still has to remember the choice for this page view, or the
  // control would drift back to the OS setting while the document does not.
  let chosen = false;

  function savedTheme(): Theme | null {
    try {
      const saved = window.localStorage.getItem(storageKey);
      if (saved === 'light' || saved === 'dark') return saved;
    } catch {
      // Storage can be blocked outright; fall back to the media query.
    }
    return null;
  }

  function toggle() {
    const next = theme === 'dark' ? 'light' : 'dark';
    theme = next;
    chosen = true;
    document.documentElement.dataset.theme = next;
    try {
      window.localStorage.setItem(storageKey, next);
    } catch {
      // Theme persistence is optional; keep the in-memory preference usable.
    }
  }

  onMount(() => {
    const saved = savedTheme();
    const media = window.matchMedia('(prefers-color-scheme: dark)');
    // Without a stored choice the document keeps following the OS setting.
    if (saved) document.documentElement.dataset.theme = saved;
    chosen = Boolean(saved);
    theme = saved ?? (media.matches ? 'dark' : 'light');
    ready = true;
    const follow = () => {
      if (!chosen && !savedTheme()) theme = media.matches ? 'dark' : 'light';
    };
    media.addEventListener('change', follow);
    return () => media.removeEventListener('change', follow);
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
