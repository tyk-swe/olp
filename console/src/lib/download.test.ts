// @vitest-environment jsdom
import { afterEach, expect, it, vi } from 'vitest';
import { downloadBlob } from '$lib/download';

afterEach(() => {
  vi.unstubAllGlobals();
});

function stubObjectUrls() {
  vi.stubGlobal('URL', {
    ...URL,
    createObjectURL: vi.fn(() => 'blob:download-1'),
    revokeObjectURL: vi.fn()
  });
}

it('clicks a named link to the blob and then revokes its URL', () => {
  stubObjectUrls();
  const blob = new Blob(['{}'], { type: 'application/json' });
  const click = vi
    .spyOn(HTMLAnchorElement.prototype, 'click')
    .mockImplementation(function (this: HTMLAnchorElement) {
      expect(this.href).toBe('blob:download-1');
      expect(this.download).toBe('result.json');
      expect(URL.revokeObjectURL).not.toHaveBeenCalled();
    });

  downloadBlob(blob, 'result.json');

  expect(URL.createObjectURL).toHaveBeenCalledWith(blob);
  expect(click).toHaveBeenCalledOnce();
  expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:download-1');
});

it('revokes the URL when the click throws', () => {
  stubObjectUrls();
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {
    throw new Error('blocked');
  });

  expect(() => downloadBlob(new Blob(['x']), 'x.bin')).toThrow('blocked');
  expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:download-1');
});
