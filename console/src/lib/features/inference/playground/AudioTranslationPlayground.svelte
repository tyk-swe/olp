<script lang="ts">
  import PublicKeyField from './PublicKeyField.svelte';
  import {
    translateAudio,
    translationFormats,
    type TranslationFormat
  } from './audioTranslation';
  import { usePublicClientAction } from './usePublicClientAction.svelte';

  let { route }: { route: string } = $props();
  let key = $state('');
  let files = $state<FileList>();
  let prompt = $state('');
  let temperature = $state('');
  let format = $state<TranslationFormat>('json');
  let result = $state<string | null>(null);
  const client = usePublicClientAction('Audio translation failed.', () => {
    key = '';
  });

  async function run(event: SubmitEvent) {
    event.preventDefault();
    const file = files?.[0];
    if (client.busy || !file) return;
    result = null;
    await client.run(
      (signal) =>
        translateAudio(route, key, file, prompt, temperature, format, signal),
      (value) => {
        result = value;
      }
    );
  }

  function clear() {
    client.reset();
    result = null;
  }
</script>

<form class="card audio-translation" onsubmit={run}>
  <p class="eyebrow">Public audio translation · billed on execution</p>
  <h2>Translate audio into English</h2>
  <p>
    The original file is uploaded to the selected route. Empty optional controls
    use the provider's defaults.
  </p>
  <PublicKeyField
    id="translation-key"
    help="Held only in this tab and sent to the same-origin public API."
    bind:value={key}
  />
  <div class="form-field">
    <label for="translation-file">Audio file</label>
    <input
      id="translation-file"
      type="file"
      accept="audio/*,.mp4,.webm"
      bind:files
    />
  </div>
  <div class="form-field">
    <label for="translation-prompt">English prompt (optional)</label>
    <textarea id="translation-prompt" bind:value={prompt} rows="3"></textarea>
  </div>
  <div class="form-field">
    <label for="translation-temperature">Temperature (optional)</label>
    <input
      id="translation-temperature"
      type="text"
      inputmode="decimal"
      bind:value={temperature}
    />
  </div>
  <div class="form-field">
    <label for="translation-format">Response format</label>
    <select id="translation-format" bind:value={format}>
      {#each translationFormats as item (item)}<option value={item}>
          {item}
        </option>{/each}
    </select>
  </div>
  <div class="actions">
    <button
      class="button button-primary"
      type="submit"
      disabled={client.busy || !key || !route || !files?.[0]}
    >
      {client.busy ? 'Translating…' : 'Run billable translation'}
    </button>
    {#if client.busy}<button
        class="button button-secondary"
        type="button"
        onclick={client.cancel}
      >
        Cancel delivery
      </button>{/if}
    {#if result !== null}<button
        class="button button-secondary"
        type="button"
        onclick={clear}
      >
        Clear result
      </button>{/if}
  </div>
  {#if client.error}<p role="alert" class="inline-problem">
      {client.error}
    </p>{/if}
  {#if result !== null}<pre
      aria-label="Native audio translation result">{result}</pre>{/if}
</form>

<style>
  .audio-translation {
    display: grid;
    gap: 1rem;
    padding: 1.5rem;
    margin-top: 1rem;
    min-width: 0;
  }
  h2,
  p {
    margin: 0;
  }
  pre {
    overflow: auto;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
  }
</style>
