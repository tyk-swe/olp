import { describe, expect, it } from 'vitest';

import { modesFor } from './routeEditor';

describe('modesFor', () => {
  it('offers webrtc as its own realtime simulation mode', () => {
    expect(modesFor('realtime')).toEqual(['realtime', 'webrtc']);
  });

  it('keeps non-realtime operations on their existing modes', () => {
    expect(modesFor('generation')).toEqual(['unary', 'streaming']);
    expect(modesFor('embeddings')).toEqual(['unary']);
  });
});
