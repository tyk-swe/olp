import { describe, expect, it } from 'vitest';
import {
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
});
