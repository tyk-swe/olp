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
    environment: 'jsdom',
    include: ['src/**/*.test.ts'],
    restoreMocks: true
  }
});
