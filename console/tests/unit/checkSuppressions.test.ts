import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { describe, expect, it, onTestFinished } from 'vitest';

const script = path.resolve('scripts/check-suppressions.mjs');
const check = (listed: readonly string[]) => {
  const directory = mkdtempSync(path.join(tmpdir(), 'suppressions-'));
  onTestFinished(() => rmSync(directory, { recursive: true }));
  writeFileSync(path.join(directory, 'present.ts'), '');
  writeFileSync(
    path.join(directory, 'eslint-suppressions.json'),
    JSON.stringify(
      Object.fromEntries(
        listed.map((file) => [file, { 'max-lines': { count: 1 } }])
      )
    )
  );
  return spawnSync(process.execPath, [script], {
    cwd: directory,
    encoding: 'utf8'
  });
};

describe('check-suppressions', () => {
  it('accepts entries whose files exist', () => {
    expect(check(['present.ts']).status).toBe(0);
  });

  it('rejects and names an entry whose file was deleted or renamed', () => {
    const result = check(['present.ts', 'removed.ts']);
    expect(result.status).toBe(1);
    expect(result.stderr).toContain('removed.ts');
    expect(result.stderr).not.toContain('present.ts');
  });
});
