<script lang="ts">
  import { resolve } from '$app/paths';
  import type { Snippet } from 'svelte';
  import BrandMark from '$lib/components/BrandMark.svelte';
  import RouteDiagram from '$lib/components/RouteDiagram.svelte';

  let { children }: { children: Snippet } = $props();
  const headline = 'One control point for every model route.'.split(' ');
</script>

<a class="skip-link" href="#setup-content">Skip to setup</a>
<main class="setup-shell" id="setup-content" tabindex="-1">
  <div class="setup-header">
    <a class="brand" href={resolve('/')} aria-label="OpenLLMProxy home">
      <BrandMark size={22} />
      <span class="wordmark">OpenLLMProxy</span>
    </a>
  </div>

  <div class="setup-body">
    <section class="setup-context" aria-labelledby="setup-introduction">
      <p class="eyebrow eyebrow-live">Private AI gateway</p>
      <h1 id="setup-introduction">
        {#each headline as word, index (index)}<span
            class="word"
            style="--word: {index}">{word}</span
          >{index < headline.length - 1 ? ' ' : ''}{/each}
      </h1>
      <p class="intro">
        Connect providers, publish stable routes, and issue installation-scoped
        keys without storing prompts or model output.
      </p>

      <div class="diagram">
        <RouteDiagram />
      </div>

      <ul aria-label="Installation properties">
        <li>
          <span aria-hidden="true">✓</span> Runs inside your infrastructure
        </li>
        <li>
          <span aria-hidden="true">✓</span> Metadata-only operational history
        </li>
        <li>
          <span aria-hidden="true">✓</span> OpenAI, Anthropic, and Gemini surfaces
        </li>
      </ul>
    </section>

    <section class="setup-stage card card-light">
      <div class="stage-inner">{@render children()}</div>
    </section>
  </div>
</main>

<style>
  .setup-shell {
    display: flex;
    min-height: 100dvh;
    flex-direction: column;
    align-items: center;
    padding: 0 1.5rem 5rem;
    background: var(--canvas);
  }

  .setup-header {
    display: flex;
    width: 100%;
    max-width: var(--page-max);
    min-height: 4rem;
    align-items: center;
    animation: fade var(--dur-enter) var(--ease-out) backwards;
  }

  .brand {
    display: inline-flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.6rem;
    color: var(--foreground);
    text-decoration: none;
  }

  .setup-body {
    display: grid;
    width: 100%;
    max-width: var(--page-max);
    flex: 1;
    grid-template-columns: minmax(0, 1fr) minmax(0, 31rem);
    align-items: center;
    gap: 4rem;
    padding: 3rem 0;
  }

  .setup-context {
    min-width: 0;
  }

  /* The introduction assembles in reading order: eyebrow, headline word by
     word, then the supporting copy, diagram and properties. */
  .setup-context > :not(h1) {
    animation-name: rise;
    animation-duration: 520ms;
    animation-fill-mode: backwards;
    animation-timing-function: var(--ease-out);
  }

  .eyebrow {
    animation-delay: 60ms;
  }

  h1 {
    max-width: 30rem;
    margin: 0;
    font-size: clamp(2rem, 4vw, var(--text-heading-lg));
    font-weight: 400;
    letter-spacing: -0.03em;
    line-height: 1.1;
  }

  .word {
    display: inline-block;
    animation: word-in 680ms var(--ease-out) backwards;
    animation-delay: calc(140ms + var(--word) * 55ms);
  }

  .intro {
    max-width: 31rem;
    margin: 1.25rem 0 0;
    color: var(--foreground-muted);
    font-size: 1rem;
    line-height: 1.5;
    animation-delay: 520ms;
  }

  /* The glow belongs to the diagram, so the copy above and below it keeps a
     solid canvas and verifiable contrast. */
  .diagram {
    position: relative;
    isolation: isolate;
    max-width: 32rem;
    margin: 2.5rem 0 0;
    padding: 1.5rem 0;
    animation-delay: 640ms;
  }

  .diagram::before {
    content: '';
    position: absolute;
    z-index: -1;
    inset: -2rem -3rem;
    background:
      radial-gradient(
        closest-side,
        color-mix(in srgb, var(--color-blue-500) 22%, transparent),
        transparent
      ),
      radial-gradient(
          circle at 1px 1px,
          color-mix(in srgb, var(--foreground) 9%, transparent) 1px,
          transparent 0
        )
        0 0 / 16px 16px;
    mask-image: radial-gradient(closest-side, #000 30%, transparent);
    pointer-events: none;
  }

  ul {
    display: grid;
    gap: 0.75rem;
    margin: 1.5rem 0 0;
    padding: 0;
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: 0.75rem;
    letter-spacing: -0.24px;
    list-style: none;
    text-transform: uppercase;
    animation-delay: 760ms;
  }

  li {
    display: flex;
    align-items: center;
    gap: 0.75rem;
  }

  li span {
    display: grid;
    width: 1.125rem;
    height: 1.125rem;
    place-items: center;
    border: 1px solid color-mix(in srgb, var(--signal) 45%, var(--border));
    border-radius: var(--radius-control);
    background: var(--accent-soft);
    color: var(--signal);
    font-size: 0.625rem;
  }

  .setup-stage {
    position: relative;
    min-width: 0;
    padding: 2rem;
    border-radius: var(--radius-panel);
    animation: stage-in 640ms var(--ease-out) 180ms backwards;
  }

  /* A signal edge along the top of the paper stage. */
  .setup-stage::before {
    content: '';
    position: absolute;
    top: -1px;
    right: 2rem;
    left: 2rem;
    height: 2px;
    border-radius: 2px;
    background: linear-gradient(
      90deg,
      transparent,
      var(--color-blue-500) 50%,
      transparent
    );
  }

  .stage-inner {
    width: 100%;
  }

  @keyframes word-in {
    from {
      opacity: 0;
      filter: blur(6px);
      transform: translateY(0.35em);
    }
  }

  @keyframes stage-in {
    from {
      opacity: 0;
      transform: translateY(16px) scale(0.985);
    }
  }

  @media (max-width: 54rem) {
    .setup-body {
      grid-template-columns: minmax(0, 31rem);
      justify-content: center;
      align-items: start;
      gap: 2rem;
      padding: 2rem 0 0;
    }

    .setup-context ul,
    .diagram {
      display: none;
    }
  }

  @media (max-width: 34rem) {
    .setup-shell {
      padding: 0 1rem 3rem;
    }

    .setup-stage {
      padding: 1.5rem;
    }
  }

  @media (forced-colors: active) {
    li span {
      border-color: CanvasText;
      color: CanvasText;
    }

    .diagram::before,
    .setup-stage::before {
      display: none;
    }
  }
</style>
