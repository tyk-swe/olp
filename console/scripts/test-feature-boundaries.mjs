import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { ESLint } from 'eslint';
import { buildFeatureBoundaryConfigs } from '../feature-boundaries.mjs';

const root = mkdtempSync(join(tmpdir(), 'olp-eslint-boundaries-'));
const features = join(root, 'src/lib/features');
const files = {
  area: 'alpha/Area.ts',
  first: 'alpha/first/First.ts',
  sibling: 'alpha/second/Second.ts',
  other: 'beta/Beta.ts'
};

try {
  for (const file of Object.values(files)) {
    const path = join(features, file);
    mkdirSync(join(path, '..'), { recursive: true });
    writeFileSync(path, 'export const fixture = true;\n');
  }

  const eslint = new ESLint({
    cwd: root,
    overrideConfigFile: true,
    overrideConfig: buildFeatureBoundaryConfigs(features, root)
  });
  const cases = [
    [files.first, "import '../Area';", false, 'parent shared import'],
    [files.area, "import './first/First';", false, 'parent composition import'],
    [files.first, "import '../second/Second';", true, 'relative sibling import'],
    [files.first, "import '$lib/features/beta/Beta';", true, 'aliased cross-area import']
  ];

  for (const [file, code, rejected, label] of cases) {
    const [result] = await eslint.lintText(code, { filePath: join(root, 'src/lib/features', file) });
    const boundaryErrors = result.messages.filter(
      (message) => message.ruleId === 'no-restricted-imports'
    );
    if ((boundaryErrors.length > 0) !== rejected) {
      throw new Error(`${label} was ${rejected ? 'accepted' : 'rejected'} unexpectedly`);
    }
  }

  console.log('feature import boundary fixtures passed');
} finally {
  rmSync(root, { recursive: true, force: true });
}
