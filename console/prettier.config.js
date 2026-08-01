import sveltePlugin from 'prettier-plugin-svelte';

export default {
  plugins: [sveltePlugin],
  singleQuote: true,
  trailingComma: 'none',
  overrides: [{ files: '*.svelte', options: { parser: 'svelte' } }]
};
