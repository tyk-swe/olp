<script lang="ts">
  import { onMount } from 'svelte';
  import NavIcon from '#lib/components/NavIcon.svelte';
  import { errorMessage } from '#lib/management/http.js';
  import { OVERVIEW } from '#lib/shell/navigationTree/pinned.js';
  import {
    addBookmark,
    bookmarkOrigin,
    removeBookmark,
    storedBookmarks,
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
    const stored = storedBookmarks(() => window.localStorage, origin);
    if (!stored.ok) {
      error = 'Installation bookmarks are unavailable in this browser.';
      return;
    }
    bookmarks = stored.value;
  });

  function add(event: SubmitEvent) {
    event.preventDefault();
    error = '';
    const result = addBookmark(
      () => window.localStorage,
      origin,
      bookmarks,
      bookmarkName,
      bookmarkURL
    );
    if (!result.ok) {
      error = errorMessage(result.error);
      return;
    }
    bookmarks = result.value;
    bookmarkName = bookmarkURL = '';
  }

  function remove(target: string) {
    const result = removeBookmark(() => window.localStorage, bookmarks, target);
    if (!result.ok) {
      error = errorMessage(result.error);
      return;
    }
    bookmarks = result.value;
    error = '';
  }

  function switchTo(target: string) {
    if (target === origin) return;

    window.location.assign(
      bookmarkOrigin(target, origin, bookmarks) + OVERVIEW.href
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
></svelte:document>

<details class="installation-switcher" bind:this={menu}>
  <summary
    aria-label={`Choose installation: ${name}`}
    title={`Manage installations: ${name}`}
  >
    <span class="name">{name}</span>
    <NavIcon name="chevron" size={14} />
  </summary>
  <div class="switcher-menu">
    <label>
      Installation<select
        aria-label="Switch installation"
        class="filter-control"
        value={origin}
        onchange={(event) => switchTo(event.currentTarget.value)}
      >
        <option value={origin}>{name} (current)</option>
        {#each bookmarks.filter((entry) => entry.origin !== origin) as entry (entry.origin)}<option
            value={entry.origin}
          >
            {entry.name} — {entry.origin}
          </option>{/each}
      </select>
    </label>
    <p class="muted">
      Switching opens that installation with its own session. Bookmarks contain
      names and origins; keys and data stay separate.
    </p>
    <form onsubmit={add}>
      <label>
        Bookmark name<input
          class="filter-control"
          bind:value={bookmarkName}
          maxlength="100"
          required
        />
      </label>
      <label>
        Installation origin<input
          class="filter-control"
          bind:value={bookmarkURL}
          type="url"
          maxlength="2048"
          placeholder="https://gateway.example.com"
          required
        />
      </label>
      <button class="button button-secondary">Add bookmark</button>
    </form>
    {#each bookmarks as entry (entry.origin)}<div class="bookmark">
        <span>{entry.name}</span><button
          class="button button-quiet"
          type="button"
          aria-label={`Remove ${entry.name} bookmark`}
          onclick={() => remove(entry.origin)}
        >
          Remove
        </button>
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
    display: inline-flex;
    max-width: 100%;
    align-items: center;
    gap: 0.25rem;
    color: var(--foreground);
    cursor: pointer;
    font-size: var(--text-body-sm);
    font-weight: 500;
    list-style: none;
  }
  summary::-webkit-details-marker {
    display: none;
  }
  .name {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  summary :global(.icon-chevron) {
    flex: none;
    color: var(--foreground-subtle);
    transition: transform var(--motion);
  }
  .installation-switcher[open] > summary :global(.icon-chevron) {
    transform: rotate(180deg);
  }
  .installation-switcher[open] .switcher-menu {
    animation: pop-in 180ms var(--ease-out);
  }
  .switcher-menu {
    color: var(--foreground);
    font-family: var(--font-sans);
    font-size: var(--text-body-sm);
    font-weight: 400;
    line-height: 1.5;
    letter-spacing: normal;
    text-transform: none;
    white-space: normal;
    position: absolute;
    z-index: 40;
    top: calc(100% + 0.75rem);
    left: 0;
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
