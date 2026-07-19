import type { Meta, StoryObj } from '@storybook/svelte';
import AsyncStateStory from './AsyncStateStory.svelte';

const meta = {
  title: 'Foundation/Async states',
  component: AsyncStateStory,
  args: { state: 'loading' },
  parameters: { layout: 'fullscreen', a11y: { test: 'error' } }
} satisfies Meta<typeof AsyncStateStory>;

export default meta;
type Story = StoryObj<typeof meta>;

export const Loading: Story = {};
export const Empty: Story = { args: { state: 'empty' } };
export const Error: Story = { args: { state: 'error' } };
