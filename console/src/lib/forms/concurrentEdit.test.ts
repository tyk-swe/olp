import { describe, expect, it } from 'vitest';
import {
  acceptRemote,
  beginReload,
  conflictNotice,
  initialConcurrentEdit,
  markConflict,
  markDirty,
  markSaved,
  reconcile
} from './concurrentEdit';

describe('concurrent edit state', () => {
  it('hydrates initially and preserves object identity when nothing changed', () => {
    const initial = reconcile(initialConcurrentEdit(), 'v1');
    expect(initial.hydrate).toBe(true);
    const unchanged = reconcile(initial.state, 'v1');
    expect(unchanged.hydrate).toBe(false);
    expect(unchanged.state).toBe(initial.state);
  });

  it('does not overwrite dirty input when a newer remote version arrives', () => {
    const hydrated = reconcile(initialConcurrentEdit(), 'v1').state;
    const dirty = markDirty(hydrated);
    const newer = reconcile(dirty, 'v2');
    expect(newer.hydrate).toBe(false);
    expect(newer.state.snapshotEtag).toBe('v1');
    expect(newer.state.remoteEtag).toBe('v2');
    expect(conflictNotice(newer.state)).toBe('newer');
  });

  it('forces hydration after a conflict even when refetch returns the cached version', () => {
    const dirty = markDirty(reconcile(initialConcurrentEdit(), 'v1').state);
    const conflicted = markConflict(dirty);
    expect(conflictNotice(conflicted)).toBe('conflict');
    const reloaded = reconcile(beginReload(conflicted), 'v1');
    expect(reloaded.hydrate).toBe(true);
    expect(reloaded.state.dirty).toBe(false);
    expect(conflictNotice(reloaded.state)).toBeNull();
  });

  it('advances self-induced versions without dropping edits and resets after save', () => {
    const dirty = markDirty(reconcile(initialConcurrentEdit(), 'v1').state);
    const accepted = acceptRemote(dirty, 'v2');
    expect(accepted.snapshotEtag).toBe('v2');
    expect(accepted.dirty).toBe(true);
    expect(conflictNotice(accepted)).toBeNull();
    expect(markSaved('v3')).toEqual({
      snapshotEtag: 'v3',
      remoteEtag: 'v3',
      dirty: false,
      conflict: false,
      reloadPending: false
    });
  });

  it('preserves external drift when a secondary mutation advances the remote version', () => {
    const dirty = markDirty(reconcile(initialConcurrentEdit(), 'v1').state);
    const drifted = reconcile(dirty, 'v2').state;
    const advanced = acceptRemote(drifted, 'v3');
    expect(advanced.snapshotEtag).toBe('v1');
    expect(advanced.remoteEtag).toBe('v3');
    expect(advanced.dirty).toBe(true);
    expect(conflictNotice(advanced)).toBe('newer');
  });

  it('preserves an explicit save conflict across a later remote refresh', () => {
    const conflicted = markConflict(markDirty(reconcile(initialConcurrentEdit(), 'v1').state));
    const refreshed = acceptRemote(conflicted, 'v2');
    expect(refreshed.snapshotEtag).toBe('v1');
    expect(refreshed.remoteEtag).toBe('v2');
    expect(conflictNotice(refreshed)).toBe('conflict');
  });

  it('hydrates a clean form when the remote version changes', () => {
    const hydrated = reconcile(initialConcurrentEdit(), 'v1').state;
    const newer = reconcile(hydrated, 'v2');
    expect(newer.hydrate).toBe(true);
    expect(newer.state.snapshotEtag).toBe('v2');
    expect(newer.state.dirty).toBe(false);
  });

  it('reloads the latest observed remote version after drift', () => {
    const dirty = markDirty(reconcile(initialConcurrentEdit(), 'v1').state);
    const drifted = reconcile(dirty, 'v2').state;
    const reloaded = reconcile(beginReload(drifted), 'v2');
    expect(reloaded.hydrate).toBe(true);
    expect(reloaded.state.snapshotEtag).toBe('v2');
    expect(conflictNotice(reloaded.state)).toBeNull();
  });

  it('keeps idempotent dirty and conflict transitions referentially stable', () => {
    const dirty = markDirty(reconcile(initialConcurrentEdit(), 'v1').state);
    expect(markDirty(dirty)).toBe(dirty);
    const conflicted = markConflict(dirty);
    expect(markConflict(conflicted)).toBe(conflicted);
  });
});
