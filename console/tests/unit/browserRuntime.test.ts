import { describe, expect, it } from 'vitest';

import { isAbortedFirefoxFontDownload } from '../browserRuntime';

describe('Firefox browser diagnostics', () => {
  it('ignores only a navigation-aborted font download', () => {
    expect(
      isAbortedFirefoxFontDownload(
        '[JavaScript Error: "downloadable font: download failed ' +
          '(font-family: JetBrains Mono): status=2152398850 source: http://example.test/font.woff2"]'
      )
    ).toBe(true);
    expect(
      isAbortedFirefoxFontDownload(
        '[JavaScript Error: "downloadable font: download failed ' +
          '(font-family: JetBrains Mono): status=2152398877 source: http://example.test/font.woff2"]'
      )
    ).toBe(false);
    expect(
      isAbortedFirefoxFontDownload(
        '[JavaScript Error: "script download failed: status=2152398850"]'
      )
    ).toBe(false);
  });
});
