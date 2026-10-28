import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';

const fixtureFiles = new Set(['scripts/check-test-ratchet.test.mjs']);

const testFile = (path) =>
  path.endsWith('_test.go') || /\.(test|spec)\.(ts|tsx|js|jsx|mts|mjs)$/.test(path);
const goldenFile = (path) => /\.golden(\.json)?$/.test(path);

const lineRules = [
  [/(^|\W)(it|test|describe|context)(\.\w+)*\.only\s*\(/, 'focused test'],
  [/(^|\W)(xit|xtest|xdescribe)\s*\(/, 'skipped test'],
  [/(^|\W)(t|b|tb|tt|f)\.Skip(f|Now)?\s*\(/, 'skipped test'],
  [/(^|\W)(it|test|describe|context)(\.\w+)*\.(skip|skipIf|todo|fixme)\s*\(/, 'skipped test'],
  [/to\w*Snapshot\s*\(/, 'snapshot assertion']
];

export function checkRatchet(changes) {
  return changes.flatMap(({ path, status, addedLines }) => {
    const golden =
      status === 'A' && goldenFile(path)
        ? [
            `${path}: new golden file: assert behavior or an invariant instead, or cite the outside source and date in a sibling README and get the citation reviewed`
          ]
        : [];
    if (!testFile(path) || fixtureFiles.has(path)) return golden;
    const lines = addedLines.flatMap((line) =>
      lineRules
        .filter(([pattern]) => pattern.test(line))
        .map(([, rule]) => `${path}: ${rule} added: ${line.trim()}`)
    );
    return [...golden, ...lines];
  });
}

export function wholeFileChanges(changes, read) {
  return changes.map((change) => {
    if (!testFile(change.path) || change.status === 'D') return change;
    const text = read(change.path);
    return text === null ? change : { ...change, addedLines: text.split('\n') };
  });
}

export function parseDiff(diff, statusByPath) {
  const changes = [];
  let current = null;
  for (const line of diff.split('\n')) {
    if (line.startsWith('+++ b/')) {
      const path = line.slice('+++ b/'.length);
      current = { path, status: statusByPath.get(path) ?? 'M', addedLines: [] };
      changes.push(current);
    } else if (line.startsWith('diff --git ')) {
      current = null;
    } else if (current && line.startsWith('+') && !line.startsWith('+++')) {
      current.addedLines.push(line.slice(1));
    }
  }
  return changes;
}

const git = (args) =>
  execFileSync('git', ['-c', 'core.quotepath=false', ...args], {
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024
  });

function mergeBase(baseRef) {
  const ref =
    baseRef ??
    ['origin/main', 'main'].find((candidate) => {
      try {
        execFileSync('git', ['rev-parse', '--verify', '--quiet', `${candidate}^{commit}`]);
        return true;
      } catch {
        return false;
      }
    });
  if (!ref) throw new Error('No base ref: pass one or fetch origin/main');
  return git(['merge-base', ref, 'HEAD']).trim();
}

if (import.meta.main) {
  const base = mergeBase(process.argv[2] ?? process.env.RATCHET_BASE);
  const statusFields = git(['diff', '--no-renames', '--name-status', '-z', base]).split('\0');
  const statusByPath = new Map();
  for (let i = 0; i + 1 < statusFields.length; i += 2)
    statusByPath.set(statusFields[i + 1], statusFields[i]);
  const tracked = wholeFileChanges(
    parseDiff(
      git(['diff', '--no-renames', '--src-prefix=a/', '--dst-prefix=b/', '-U0', base]),
      statusByPath
    ),
    (path) => (existsSync(path) ? readFileSync(path, 'utf8') : null)
  );
  const trackedPaths = new Set(tracked.map((c) => c.path));
  const untracked = git(['ls-files', '-o', '--exclude-standard'])
    .split('\n')
    .filter((path) => path && !trackedPaths.has(path) && (testFile(path) || goldenFile(path)))
    .map((path) => ({
      path,
      status: 'A',
      addedLines: readFileSync(path, 'utf8').split('\n')
    }));
  const violations = checkRatchet([...tracked, ...untracked]);
  for (const violation of violations) console.error(`test-ratchet: ${violation}`);
  if (violations.length > 0) process.exit(1);
  console.log(`test-ratchet: no violations against ${base.slice(0, 12)}`);
}
