// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { downloadBlob } from './download';

const createObjectURL = vi.fn<typeof URL.createObjectURL>();
const revokeObjectURL = vi.fn<typeof URL.revokeObjectURL>();

beforeEach(() => {
  createObjectURL.mockReturnValue('blob:download');
  vi.stubGlobal(
    'URL',
    class extends URL {
      static createObjectURL = createObjectURL;
      static revokeObjectURL = revokeObjectURL;
    }
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
});

it('downloads the Blob with the requested filename without attaching its link', () => {
  const blob = new Blob(['result'], { type: 'application/json' });
  const click = vi
    .spyOn(HTMLAnchorElement.prototype, 'click')
    .mockImplementation(function (this: HTMLAnchorElement) {
      expect(this.href).toBe('blob:download');
      expect(this.download).toBe('result.json');
      expect(this.isConnected).toBe(false);
      expect(revokeObjectURL).not.toHaveBeenCalled();
    });

  downloadBlob(blob, 'result.json');

  expect(createObjectURL).toHaveBeenCalledWith(blob);
  expect(click).toHaveBeenCalledOnce();
  expect(revokeObjectURL).toHaveBeenCalledExactlyOnceWith('blob:download');
});

it('releases the temporary URL when clicking the download link fails', () => {
  const error = new Error('Download failed');
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {
    throw error;
  });

  expect(() => downloadBlob(new Blob(['result']), 'result.json')).toThrow(
    error
  );
  expect(revokeObjectURL).toHaveBeenCalledExactlyOnceWith('blob:download');
});
