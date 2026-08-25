import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vitest/config';

export default defineConfig({
  build: {
    // Keep packaged fonts compatible with the production `font-src 'self'`
    // policy even when a small Unicode subset falls below Vite's inline limit.
    assetsInlineLimit: 0
  },
  plugins: [sveltekit()],
  test: {
    clearMocks: true,
    restoreMocks: true,
    projects: [
      {
        extends: true,
        test: {
          name: 'unit',
          include: ['src/**/*.test.ts'],
          exclude: ['src/**/*.svelte.test.ts']
        }
      },
      {
        // Runes and component lifecycles only exist in Svelte's client build,
        // which needs both a DOM and the browser export conditions.
        extends: true,
        resolve: { conditions: ['browser'] },
        test: {
          name: 'client',
          environment: 'jsdom',
          include: ['src/**/*.svelte.test.ts']
        }
      }
    ]
  }
});
