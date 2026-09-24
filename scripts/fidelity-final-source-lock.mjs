import { createHash } from 'node:crypto';
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

export const priorProduct = 'bc325da50c575803c771531dc4e3aab1ac203a53';
export const lockPath = 'docs/evidence/fidelity-performance/final-source-repeat-v1/product-lock.json';
export const helperPath = 'scripts/fidelity-final-source-lock.mjs';
export const postProductEvidencePrefix = 'docs/evidence/fidelity-performance/';
export const sha256 = (value) => createHash('sha256').update(value).digest('hex');
export const fileSHA256 = (path) => sha256(readFileSync(path));
export function gitBlobSHA256(revision, path) {
  const result = spawnSync('git', ['show', `${revision}:${path}`], { maxBuffer: 32 << 20 });
  if (result.status !== 0) throw new Error(`Missing committed source: ${path}`);
  return sha256(result.stdout);
}

export function git(args) {
  const result = spawnSync('git', args, { encoding: 'utf8', maxBuffer: 32 << 20 });
  if (result.status !== 0) throw new Error(`git ${args[0]} failed: ${result.stderr?.trim() || result.status}`);
  return result.stdout.trim();
}
export function ancestor(older, newer) {
  return older !== newer && spawnSync('git', ['merge-base', '--is-ancestor', older, newer]).status === 0;
}
export function sourceRevision() { return git(['rev-parse', 'HEAD']); }
export function requireClean() {
  if (git(['status', '--short']) !== '') throw new Error('A clean source worktree is required');
}
export function committedOnce(path, source = sourceRevision()) {
  if (!existsSync(path) || git(['status', '--short', '--', path]) !== '') throw new Error(`Missing or dirty committed evidence: ${path}`);
  git(['ls-files', '--error-unmatch', path]);
  const history = git(['log', '--format=%H', '--', path]).split('\n').filter(Boolean);
  if (history.length !== 1 || !ancestor(history[0], source)) throw new Error(`Evidence was edited or is not a strict ancestor: ${path}`);
  if (git(['hash-object', path]) !== git(['rev-parse', `${source}:${path}`])) {
    throw new Error(`Committed evidence bytes differ: ${path}`);
  }
  return history[0];
}

// Pin every production Go blob, not only the three connector files changed by
// the profile hot-path fix. Test fixtures and measurement adapters stay pinned
// separately by their own methods.
export function productionGoManifest(revision) {
  const raw = spawnSync('git', ['ls-tree', '-r', '-z', revision, '--', 'internal', 'cmd', 'openapi'], { maxBuffer: 32 << 20 });
  if (raw.status !== 0) throw new Error('Cannot enumerate product Go blobs');
  return raw.stdout.toString('utf8').split('\0').filter(Boolean).map((record) => {
    const [entry, path] = record.split('\t');
    const blob = entry.split(' ')[2];
    return { path, blob };
  }).filter(({ path }) => path.endsWith('.go') && !path.endsWith('_test.go')).sort((a, b) => a.path < b.path ? -1 : a.path > b.path ? 1 : 0);
}
export function productDigest(revision) { return sha256(JSON.stringify(productionGoManifest(revision))); }

// A move across the evidence boundary must report both its deleted source and
// added destination. Git's default rename detection can hide the source path.
export function changedPaths(from, to, paths = [], cwd = process.cwd()) {
  const args = ['diff', '--no-renames', '--name-only', '-z', from, to];
  if (paths.length) args.push('--', ...paths);
  const result = spawnSync('git', args, { cwd, maxBuffer: 32 << 20 });
  if (result.status !== 0) throw new Error(`Cannot compare locked source trees: ${result.stderr?.toString().trim() || result.status}`);
  return result.stdout.toString('utf8').split('\0').filter(Boolean);
}

export function assertOnlyEvidenceChanges(paths) {
  const changed = paths.filter(Boolean);
  const forbidden = changed.filter((path) => !path.startsWith(postProductEvidencePrefix));
  if (forbidden.length) throw new Error(`Tracked build or method input changed after P10: ${forbidden.join(', ')}`);
  return changed;
}

export function freezeProductLock(productRevision) {
  requireClean();
  const revision = git(['rev-parse', productRevision]);
  if (!/^[a-f0-9]{40}$/.test(revision) || !ancestor(priorProduct, revision) ||
      !(revision === sourceRevision() || ancestor(revision, sourceRevision()))) {
    throw new Error('Choose one descendant P10 product revision already in this source history');
  }
  assertOnlyEvidenceChanges(changedPaths(revision, sourceRevision()));
  const changed = changedPaths(priorProduct, revision, ['internal', 'cmd', 'openapi']).filter((path) => path.endsWith('.go') && !path.endsWith('_test.go'));
  if (!changed.includes('internal/connectors/profiles.go')) throw new Error('The P10 profile-cost product change is absent');
  const lock = { schema: 'openllmproxy.dev/final-source-product-lock/v1', prior_product_revision: priorProduct,
    product_revision: revision, production_go_sha256: productDigest(revision), production_go_files: productionGoManifest(revision).length,
    changed_product_go: changed.sort(), post_product_evidence_prefix: postProductEvidencePrefix };
  writeFileSync(lockPath, `${JSON.stringify(lock, null, 2)}\n`, { flag: 'wx' });
  return lock;
}

export function verifyProductLock(source = sourceRevision()) {
  const lockCommit = committedOnce(lockPath, source);
  const lock = JSON.parse(readFileSync(lockPath, 'utf8'));
  assertOnlyEvidenceChanges(changedPaths(lock.product_revision, source));
  if (lock.schema !== 'openllmproxy.dev/final-source-product-lock/v1' || lock.prior_product_revision !== priorProduct ||
      lock.post_product_evidence_prefix !== postProductEvidencePrefix ||
      !ancestor(priorProduct, lock.product_revision) || !ancestor(lock.product_revision, lockCommit) ||
      lock.production_go_sha256 !== productDigest(lock.product_revision) || lock.production_go_sha256 !== productDigest(source) ||
      lock.production_go_files !== productionGoManifest(source).length ||
      JSON.stringify(lock.changed_product_go) !== JSON.stringify(changedPaths(priorProduct, lock.product_revision, ['internal', 'cmd', 'openapi']).filter((path) => path.endsWith('.go') && !path.endsWith('_test.go')).sort())) {
    throw new Error('Current candidate is not the locked P10 product-Go source');
  }
  return { ...lock, lock_commit: lockCommit, lock_sha256: fileSHA256(lockPath) };
}

export function verifyMethod(path, source = sourceRevision()) {
  const method = committedOnce(path, source);
  if (gitBlobSHA256(method, path) !== fileSHA256(path)) throw new Error('Measurement method changed after registration');
  return method;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const [action, revision, extra] = process.argv.slice(2);
    if (action === 'freeze' && revision && !extra) console.log(JSON.stringify(freezeProductLock(revision), null, 2));
    else if (action === 'verify' && !revision) console.log(JSON.stringify(verifyProductLock(), null, 2));
    else throw new Error('Usage: freeze <exact-P10-revision> | verify');
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
