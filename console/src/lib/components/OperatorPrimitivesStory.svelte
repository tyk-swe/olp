<script lang="ts">
  import CursorPagination from './CursorPagination.svelte';
  import SecretDialog from './SecretDialog.svelte';
  import SegmentedRadioGroup from './SegmentedRadioGroup.svelte';
  import ThemeToggle from './ThemeToggle.svelte';

  let selected = $state('text');
  let page = $state(1);
  let dialogOpen = $state(false);
</script>

<main>
  <p class="eyebrow">Component fixture</p>
  <h1>Operator primitives</h1>
  <p class="description">Keyboard, focus, theme, pagination, and modal behavior share production primitives.</p>

  <section class="card control" aria-labelledby="mode-heading">
    <h2 id="mode-heading">Playground mode</h2>
    <SegmentedRadioGroup
      label="Response type"
      name="response-type"
      value={selected}
      items={[{ value: 'text', label: 'Text' }, { value: 'tools', label: 'Tools' }, { value: 'structured', label: 'Structured' }]}
      onChange={(value) => selected = value}
    />
    <p aria-live="polite">Selected mode: <strong>{selected}</strong></p>
  </section>

  <section class="card control" aria-labelledby="paging-heading">
    <h2 id="paging-heading">Cursor results</h2>
    <CursorPagination
      {page}
      hasPrevious={page > 1}
      hasNext={page < 3}
      onPrevious={() => page -= 1}
      onNext={() => page += 1}
      label="Fixture result pages"
    />
  </section>

  <div class="actions">
    <ThemeToggle />
    <button class="button" type="button" onclick={() => dialogOpen = true}>Reveal generated key</button>
  </div>

  {#if dialogOpen}
    <SecretDialog
      eyebrow="One-time secret"
      title="Copy this key now"
      description="The secret is displayed once and is not persisted by the console."
      onClose={() => dialogOpen = false}
    >
      {#snippet children(close)}
        <code>olp_test_once_only</code>
        <button class="button" type="button" onclick={close}>I saved the key</button>
      {/snippet}
    </SecretDialog>
  {/if}
</main>

<style>
  main { width: min(44rem, calc(100vw - 2rem)); padding: 1rem; }
  h1, h2 { margin: 0; }
  h1 { font-size: 1.75rem; }
  h2 { margin-bottom: .8rem; font-size: 1rem; }
  .description { color: var(--foreground-muted); }
  .control { margin-top: 1rem; padding: 1rem; }
  .control > p { margin: .75rem 0 0; color: var(--foreground-muted); }
  .actions { display: flex; align-items: center; justify-content: space-between; gap: 1rem; margin-top: 1rem; }
  code { display: block; margin: 1rem 0; padding: .8rem; overflow-wrap: anywhere; background: var(--surface-subtle); }
</style>
