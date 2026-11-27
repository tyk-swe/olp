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

describe('concurrentEdit state machine', () => {
  it('hydrates on first reconcile and stays idle when the ETag is unchanged', () => {
    const initial = initialConcurrentEdit();
    expect(conflictNotice(initial)).toBeNull();

    const first = reconcile(initial, 'etag-1');
    expect(first.hydrate).toBe(true);
    expect(first.state).toEqual({
      snapshotEtag: 'etag-1',
      remoteEtag: 'etag-1',
      dirty: false,
      conflict: false,
      reloadPending: false
    });

    const repeat = reconcile(first.state, 'etag-1');
    expect(repeat.hydrate).toBe(false);
    expect(repeat.state).toBe(first.state);
  });

  it('auto-rehydrates clean edits on newer remote ETag, but preserves dirty edits and signals newer', () => {
    const clean = reconcile(initialConcurrentEdit(), 'etag-1').state;
    const refreshed = reconcile(clean, 'etag-2');
    expect(refreshed.hydrate).toBe(true);
    expect(refreshed.state.snapshotEtag).toBe('etag-2');
    expect(conflictNotice(refreshed.state)).toBeNull();

    const dirty = markDirty(refreshed.state);
    expect(markDirty(dirty)).toBe(dirty);

    const behind = reconcile(dirty, 'etag-3');
    expect(behind.hydrate).toBe(false);
    expect(behind.state.snapshotEtag).toBe('etag-2');
    expect(behind.state.remoteEtag).toBe('etag-3');
    expect(conflictNotice(behind.state)).toBe('newer');
  });

  it('prioritizes conflict notice over newer and forces hydration on beginReload', () => {
    const dirty = markDirty(reconcile(initialConcurrentEdit(), 'etag-1').state);
    const newer = reconcile(dirty, 'etag-2').state;
    const conflicted = markConflict(newer);
    expect(markConflict(conflicted)).toBe(conflicted);
    expect(conflictNotice(conflicted)).toBe('conflict');

    const reloading = beginReload(conflicted);
    expect(beginReload(reloading)).toBe(reloading);

    const reloaded = reconcile(reloading, 'etag-2');
    expect(reloaded.hydrate).toBe(true);
    expect(reloaded.state).toEqual({
      snapshotEtag: 'etag-2',
      remoteEtag: 'etag-2',
      dirty: false,
      conflict: false,
      reloadPending: false
    });
    expect(conflictNotice(reloaded.state)).toBeNull();
  });

  it('advances snapshotEtag via acceptRemote only when not already conflicted or behind, and clears conflict on markSaved', () => {
    const aligned = markDirty(
      reconcile(initialConcurrentEdit(), 'etag-1').state
    );
    const accepted = acceptRemote(aligned, 'etag-2');
    expect(accepted.snapshotEtag).toBe('etag-2');
    expect(accepted.remoteEtag).toBe('etag-2');
    expect(accepted.dirty).toBe(true);
    expect(conflictNotice(accepted)).toBeNull();

    const diverged = reconcile(accepted, 'etag-3').state;
    const stillBehind = acceptRemote(diverged, 'etag-4');
    expect(stillBehind.snapshotEtag).toBe('etag-2');
    expect(stillBehind.remoteEtag).toBe('etag-4');
    expect(conflictNotice(stillBehind)).toBe('newer');

    const saved = markSaved('etag-5', true);
    expect(saved).toEqual({
      snapshotEtag: 'etag-5',
      remoteEtag: 'etag-5',
      dirty: true,
      conflict: false,
      reloadPending: false
    });
    expect(conflictNotice(saved)).toBeNull();
  });
});
