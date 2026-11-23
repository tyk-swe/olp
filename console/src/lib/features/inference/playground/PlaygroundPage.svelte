<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import {
    RoutingDecisions,
    decisionRows
  } from '#lib/features/routes/index.js';
  import RealtimeTrace from './RealtimeTrace.svelte';
  import StrictToolPlayground from './StrictToolPlayground.svelte';
  import NativeOperationPlayground from './NativeOperationPlayground.svelte';
  import AudioTranslationPlayground from './AudioTranslationPlayground.svelte';
  import { PlaygroundState } from './playgroundState.svelte';
  import PlaygroundComposer from './PlaygroundComposer.svelte';
  import PlaygroundResult from './PlaygroundResult.svelte';

  const playground = new PlaygroundState();
</script>

<PageTitle title="Playground" />

<PageHeader
  title="Playground"
  description="Test an active route from the console. Basic runs use your signed-in session."
/>
<div class="privacy-note">
  <span aria-hidden="true">◉</span>
  <p>
    <strong>Request retention</strong><br />When request metadata retention is
    enabled, Request Explorer can show route, timing, and usage information.
    Standard runs keep prompts and outputs out of request history. Negotiated
    tool recovery can retain the source request and original response encrypted
    for up to 24 hours.
  </p>
</div>

<div class="playground-grid">
  <PlaygroundComposer {playground} />

  <PlaygroundResult {playground} />
</div>

{#if playground.plan.kind === 'native'}
  {#key `${playground.model.trim()}:${playground.plan.operation}:${playground.templateKey}`}
    <NativeOperationPlayground
      route={playground.model.trim()}
      operation={playground.plan.operation}
      requestText={playground.rawJson}
      bind:dialect={playground.nativeDialect}
    />
  {/key}
{:else if playground.plan.kind === 'strict'}
  {#key playground.model.trim()}
    <StrictToolPlayground
      route={playground.model.trim()}
      requestText={playground.rawJson}
    />
  {/key}
{:else if playground.plan.kind === 'unavailable'}
  <section class="card strict-client-unavailable" role="status">
    This route needs a native or negotiated public client. Choose Advanced,
    Generation, OpenAI and the negotiated tool template for the currently
    qualified tool workflow.
  </section>
{:else if playground.plan.kind === 'audio'}
  {#key playground.model.trim()}<AudioTranslationPlayground
      route={playground.model.trim()}
    />{/key}
{:else if playground.plan.kind === 'realtime'}
  <RealtimeTrace />
{/if}

{#if playground.explanation}<section class="card composer">
    <h2>Routing explanation</h2>
    <p class="explanation-source">
      {playground.explanation.dryRun
        ? 'Dry run against the published runtime. No provider request was sent.'
        : 'From the request that just ran.'}
    </p>
    <RoutingDecisions rows={decisionRows(playground.explanation.decisions)} />
  </section>{/if}

<style>
  .strict-client-unavailable {
    padding: 1.25rem;
    margin-top: 1rem;
  }
  .explanation-source {
    margin: 0 0 1rem;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .privacy-note {
    display: flex;
    align-items: flex-start;
    gap: 0.75rem;
    margin-top: 1.25rem;
    padding: 0.85rem 1rem;
    border: 1px solid var(--success);
    border-radius: var(--radius-control);
    background: var(--success-soft);
    color: var(--foreground);
    line-height: 1.5;
  }
  .privacy-note span,
  .privacy-note strong {
    color: var(--success);
  }
  .privacy-note p {
    margin: 0;
  }
  .playground-grid {
    display: grid;
    grid-template-columns: minmax(22rem, 0.9fr) minmax(24rem, 1.1fr);
    gap: 1rem;
    margin-top: 1rem;
    align-items: start;
  }
  @media (max-width: 68rem) {
    .playground-grid {
      grid-template-columns: 1fr;
    }
  }
</style>
