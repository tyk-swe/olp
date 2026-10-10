<script lang="ts">
  import { onMount } from 'svelte';
  import { resolve } from '$app/paths';
  import { errorMessage } from '$lib/api/http';
  import {
    bookmarkOrigin,
    readInstallations,
    saveInstallations,
    type InstallationBookmark
  } from './installations';
  let { name }: { name: string } = $props();
  let bookmarks = $state<InstallationBookmark[]>([]);
  let origin = $state('');
  let bookmarkName = $state('');
  let bookmarkURL = $state('');
  let error = $state('');
  let menu = $state<HTMLDetailsElement>();
  onMount(() => {
    origin = window.location.origin;
    try {
      const stored = readInstallations(window.localStorage);
      for (const entry of stored) {
        try {
          bookmarkOrigin(entry.origin, origin, bookmarks);
          bookmarks.push(entry);
        } catch {
          /* Ignore stale bookmarks that would share installation cookies. */
        }
      }
    } catch {
      error = 'Installation bookmarks are unavailable in this browser.';
    }
  });

  function add(event: SubmitEvent) {
    event.preventDefault();
    error = '';
    try {
      const target = bookmarkOrigin(bookmarkURL, origin, bookmarks);
      const label = bookmarkName.trim();
      if (!label || label.length > 100)
        throw new Error('Use a name of 1–100 characters.');
      const next = [
        ...bookmarks.filter((entry) => entry.origin !== target),
        { name: label, origin: target }
      ];
      if (next.length > 20)
        throw new Error('Keep at most 20 installation bookmarks.');
      saveInstallations(window.localStorage, next);
      bookmarks = next;
      bookmarkName = bookmarkURL = '';
    } catch (cause) {
      error = errorMessage(cause);
    }
  }

  function remove(target: string) {
    try {
      const next = bookmarks.filter((entry) => entry.origin !== target);
      saveInstallations(window.localStorage, next);
      bookmarks = next;
      error = '';
    } catch (cause) {
      error = errorMessage(cause);
    }
  }

  function switchTo(target: string) {
    if (target === origin) return;
    window.location.assign(
      bookmarkOrigin(target, origin, bookmarks) + resolve('/overview')
    );
  }
</script>

<svelte:document
  onpointerdown={(event) => {
    if (
      menu?.open &&
      event.target instanceof Node &&
      !menu.contains(event.target)
    )
      menu.open = false;
  }}
  onkeydown={(event) => {
    if (event.key === 'Escape' && menu?.open) {
      menu.open = false;
      menu.querySelector('summary')?.focus();
    }
  }}
/>

<details class="installation-switcher" bind:this={menu}>
  <summary
    aria-label={`Choose installation: ${name}`}
    title={`Manage installations: ${name}`}
    >{name}<span aria-hidden="true"> ▾</span></summary
  >
  <div class="switcher-menu">
    <label
      >Installation<select
        aria-label="Switch installation"
        class="filter-control"
        value={origin}
        onchange={(event) => switchTo(event.currentTarget.value)}
      >
        <option value={origin}>{name} (current)</option>
        {#each bookmarks.filter((entry) => entry.origin !== origin) as entry (entry.origin)}<option
            value={entry.origin}>{entry.name} — {entry.origin}</option
          >{/each}
      </select></label
    >
    <p class="helper">
      Switching opens that installation with its own session. Bookmarks contain
      names and origins; keys and data stay separate.
    </p>
    <form onsubmit={add}>
      <label
        >Bookmark name<input
          class="filter-control"
          bind:value={bookmarkName}
          maxlength="100"
          required
        /></label
      >
      <label
        >Installation origin<input
          class="filter-control"
          bind:value={bookmarkURL}
          type="url"
          maxlength="2048"
          placeholder="https://gateway.example.com"
          required
        /></label
      >
      <button class="button button-secondary">Add bookmark</button>
    </form>
    {#each bookmarks as entry (entry.origin)}<div class="bookmark">
        <span>{entry.name}</span><button
          class="button button-quiet"
          type="button"
          aria-label={`Remove ${entry.name} bookmark`}
          onclick={() => remove(entry.origin)}>Remove</button
        >
      </div>{/each}
    {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
  </div>
</details>

<style>
  .installation-switcher {
    position: relative;
    min-width: 0;
  }
  summary {
    color: var(--foreground-muted);
    cursor: pointer;
    font-size: 0.75rem;
    max-width: 12rem;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .switcher-menu {
    color: var(--foreground);
    font-family: var(--font-sans);
    font-size: 0.875rem;
    font-weight: 400;
    line-height: 1.5;
    letter-spacing: normal;
    text-transform: none;
    white-space: normal;
    position: absolute;
    z-index: 40;
    top: calc(100% + 1rem);
    right: 0;
    width: min(22rem, 90vw);
    max-height: 75vh;
    overflow-y: auto;
    background: var(--surface-raised);
    border: 1px solid var(--border);
    border-radius: var(--radius-card);
    padding: 1rem;
    box-shadow: var(--shadow-overlay);
  }
  form,
  label {
    display: grid;
    gap: 0.5rem;
  }
  form {
    margin-top: 1rem;
  }
  input,
  select {
    min-width: 0;
    width: 100%;
  }
  .bookmark {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.5rem;
    margin-top: 0.5rem;
  }
  .bookmark > span {
    min-width: 0;
    flex: 1;
    overflow-wrap: anywhere;
  }
  @media (max-width: 40rem) {
    summary {
      max-width: 6rem;
    }
    .switcher-menu {
      position: fixed;
      inset: 4rem 1rem auto;
      width: auto;
    }
  }
</style>
