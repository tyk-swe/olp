import { describe, expect, it } from 'vitest';
import {
  cursorPaginationProps,
  emptyCursorHistory,
  popCursor,
  pushCursor,
  resetCursor
} from './pagination';

describe('cursor history', () => {
  it('moves forward and back through opaque cursors', () => {
    const state = emptyCursorHistory();

    pushCursor(state, 'second');
    pushCursor(state, 'third');
    expect(state).toEqual({
      cursor: 'third',
      history: [undefined, 'second']
    });

    popCursor(state);
    expect(state).toEqual({ cursor: 'second', history: [undefined] });
    popCursor(state);
    expect(state).toEqual({ cursor: undefined, history: [] });
  });

  it('ignores missing next cursors and resets pagination', () => {
    const state = emptyCursorHistory();

    pushCursor(state, null);
    pushCursor(state, undefined);
    expect(state).toEqual({ cursor: undefined, history: [] });

    pushCursor(state, 'second');
    resetCursor(state);
    expect(state).toEqual({ cursor: undefined, history: [] });
  });

  it('maps cursor state to the shared pagination controls', () => {
    const state = emptyCursorHistory();
    let changes = 0;

    let controls = cursorPaginationProps(state, 'second', () => changes++);
    expect(controls).toMatchObject({
      page: 1,
      hasPrevious: false,
      hasNext: true
    });
    controls.onPrevious();
    expect(changes).toBe(0);

    controls.onNext();
    expect(state).toEqual({ cursor: 'second', history: [undefined] });
    expect(changes).toBe(1);

    controls = cursorPaginationProps(state, null, () => changes++);
    expect(controls).toMatchObject({
      page: 2,
      hasPrevious: true,
      hasNext: false
    });
    controls.onNext();
    expect(changes).toBe(1);
    controls.onPrevious();
    expect(state).toEqual({ cursor: undefined, history: [] });
    expect(changes).toBe(2);
  });
});
