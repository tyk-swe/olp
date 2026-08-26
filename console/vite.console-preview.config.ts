import { defineConfig } from 'vite';

/**
 * Serves the adapter-static output in `build/` for CI e2e runs, so browser
 * jobs exercise the shipped bundle instead of a dev server. The default
 * appType ('spa') provides the index.html fallback the client-only console
 * relies on. Run via:
 *
 *   vite preview --config vite.console-preview.config.ts
 */
export default defineConfig({
  build: { outDir: 'build' }
});
