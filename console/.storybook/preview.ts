import type { Preview } from '@storybook/svelte';
import '../src/app.css';

const preview: Preview = {
  parameters: {
    a11y: { test: 'error' },
    backgrounds: { disable: true },
    controls: { expanded: true },
    layout: 'centered'
  }
};

export default preview;
