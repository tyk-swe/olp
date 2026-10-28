import { defineConfig } from 'vitest/config';
import base from './vite.config';

const unit = (base.test?.projects ?? []).find(
  (project) => typeof project === 'object' && project.test?.name === 'unit'
);
if (!unit) throw new Error('vite.config.ts declares no Vitest unit project');

export default defineConfig({
  ...base,
  test: { ...base.test, projects: [unit] }
});
