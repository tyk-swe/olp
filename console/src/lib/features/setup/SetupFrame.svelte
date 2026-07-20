<script lang="ts">
  import type { Snippet } from 'svelte';
  import BrandMark from '$lib/components/BrandMark.svelte';

  let { children }: { children: Snippet } = $props();
</script>

<a class="skip-link" href="#setup-content">Skip to setup</a>
<main class="setup-shell" id="setup-content" tabindex="-1">
  <section class="setup-context" aria-labelledby="setup-introduction">
    <a class="brand" href="/" aria-label="OpenLLMProxy home">
      <BrandMark size={36} />
      <span>OpenLLMProxy</span>
    </a>

    <div class="intro-copy">
      <p class="eyebrow">Private AI gateway</p>
      <h1 id="setup-introduction">One control point for every model route.</h1>
      <p>
        Connect providers, publish stable routes, and issue installation-scoped keys without storing
        prompts or model output.
      </p>
    </div>

    <ul aria-label="Installation properties">
      <li><span aria-hidden="true">✓</span> Runs inside your infrastructure</li>
      <li><span aria-hidden="true">✓</span> Metadata-only operational history</li>
      <li><span aria-hidden="true">✓</span> OpenAI, Anthropic, and Gemini surfaces</li>
    </ul>
  </section>

  <section class="setup-stage">
    <div class="stage-inner">{@render children()}</div>
  </section>
</main>

<style>
  .setup-shell {
    display: grid;
    min-height: 100dvh;
    grid-template-columns: minmax(18rem, 0.85fr) minmax(28rem, 1.15fr);
    background: var(--canvas);
  }

  .setup-context {
    display: flex;
    min-height: 100dvh;
    flex-direction: column;
    padding: clamp(2rem, 5vw, 4.5rem);
    border-right: 1px solid var(--sidebar-border);
    background:
      radial-gradient(circle at 15% 10%, rgb(23 103 207 / 32%), transparent 34rem),
      linear-gradient(160deg, #0d2246 0%, var(--sidebar-bg) 58%);
  }

  .brand {
    display: inline-flex;
    min-height: 2.75rem;
    align-items: center;
    align-self: flex-start;
    gap: 0.75rem;
    color: var(--sidebar-foreground-strong);
    font-size: 1.05rem;
    font-weight: 730;
    letter-spacing: -0.02em;
    text-decoration: none;
  }

  .brand {
    --accent: #4d8bfd;
  }

  .setup-context .eyebrow {
    color: #7fa9f2;
  }

  .intro-copy {
    max-width: 34rem;
    margin: auto 0;
    padding: 5rem 0;
  }

  h1 {
    max-width: 30rem;
    margin: 0;
    color: var(--sidebar-foreground-strong);
    font-size: clamp(2rem, 3.6vw, 3.25rem);
    font-weight: 740;
    letter-spacing: -0.04em;
    line-height: 1.05;
  }

  .intro-copy > p:last-child {
    max-width: 31rem;
    margin: 1.25rem 0 0;
    color: var(--sidebar-foreground);
    font-size: 1rem;
  }

  ul {
    display: grid;
    gap: 0.6rem;
    margin: 0;
    padding: 0;
    color: var(--sidebar-foreground);
    font-size: 0.8125rem;
    list-style: none;
  }

  li {
    display: flex;
    align-items: center;
    gap: 0.6rem;
  }

  li span {
    display: grid;
    width: 1.3rem;
    height: 1.3rem;
    place-items: center;
    border-radius: 50%;
    background: rgb(23 103 207 / 28%);
    color: #9dbef7;
    font-size: 0.7rem;
    font-weight: 800;
  }

  .setup-stage {
    display: grid;
    min-width: 0;
    place-items: center;
    padding: clamp(1.25rem, 5vw, 5rem);
    background: var(--surface);
  }

  .stage-inner {
    width: min(100%, 31rem);
  }

  @media (max-width: 54rem) {
    .setup-shell {
      display: block;
    }

    .setup-context {
      min-height: auto;
      padding: 1.25rem clamp(1.25rem, 5vw, 3rem) 2rem;
    }

    .intro-copy {
      margin: 0;
      padding: 3.5rem 0 2rem;
    }

    .setup-context ul {
      display: none;
    }

    .setup-stage {
      min-height: 55dvh;
      place-items: start center;
      padding-top: 2.25rem;
      padding-bottom: 4rem;
    }
  }

  @media (max-width: 34rem) {
    .intro-copy {
      padding-top: 2.25rem;
    }

    h1 {
      font-size: 2rem;
    }
  }

  @media (forced-colors: active) {
    .setup-context {
      border-right: 1px solid CanvasText;
      background: Canvas;
    }

    .setup-context .eyebrow,
    .brand,
    h1,
    .intro-copy > p:last-child,
    ul {
      color: CanvasText;
    }

    li span {
      border: 1px solid CanvasText;
      background: Canvas;
      color: CanvasText;
    }
  }
</style>
