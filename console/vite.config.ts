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
    // Local-time conversions are asserted as literal instants, which only means
    // anything if every machine runs the suite in one zone. New York is a zone
    // with a non-zero offset and daylight saving, so a UTC-only assumption
    // cannot hide in a passing test.
    env: { TZ: 'America/New_York' },
    projects: [
      {
        extends: true,
        test: {
          name: 'unit',
          include: ['src/**/*.test.ts', 'tests/unit/**/*.test.ts'],
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
