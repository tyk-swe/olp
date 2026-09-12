<script lang="ts">
  import { resolve } from '$app/paths';
  import type { Snippet } from 'svelte';
  import BrandMark from '$lib/components/BrandMark.svelte';

  let { children }: { children: Snippet } = $props();
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
      <h1 id="setup-introduction">One control point for every model route.</h1>
      <p class="intro">
        Connect providers, publish stable routes, and issue installation-scoped
        keys without storing prompts or model output.
      </p>

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

  h1 {
    max-width: 30rem;
    margin: 0;
    font-size: clamp(2rem, 4vw, var(--text-heading-lg));
    font-weight: 400;
    letter-spacing: -0.025em;
    line-height: 1.12;
  }

  .intro {
    max-width: 31rem;
    margin: 1.25rem 0 0;
    color: var(--foreground-muted);
    font-size: 1rem;
    line-height: 1.5;
  }

  ul {
    display: grid;
    gap: 0.75rem;
    margin: 2.5rem 0 0;
    padding: 0;
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: 0.75rem;
    letter-spacing: -0.24px;
    list-style: none;
    text-transform: uppercase;
  }

  li {
    display: flex;
    align-items: center;
    gap: 0.75rem;
  }

  li span {
    color: var(--metric);
  }

  .setup-stage {
    min-width: 0;
    padding: 2rem;
    border-radius: var(--radius-panel);
  }

  .stage-inner {
    width: 100%;
  }

  @media (max-width: 54rem) {
    .setup-body {
      grid-template-columns: minmax(0, 31rem);
      justify-content: center;
      align-items: start;
      gap: 2rem;
      padding: 2rem 0 0;
    }

    .setup-context ul {
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
      color: CanvasText;
    }
  }
</style>
