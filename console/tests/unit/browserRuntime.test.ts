import { describe, expect, it } from 'vitest';

import { isAbortedFirefoxFontDownload } from '../browserRuntime';

describe('Firefox browser diagnostics', () => {
  it('ignores only a navigation-aborted font download', () => {
    expect(
      isAbortedFirefoxFontDownload(
        'firefox',
        '[JavaScript Error: "downloadable font: download failed ' +
          '(font-family: JetBrains Mono): status=2152398850 source: http://example.test/font.woff2"]'
      )
    ).toBe(true);
    expect(
      isAbortedFirefoxFontDownload(
        'firefox',
        '[JavaScript Error: "downloadable font: download failed ' +
          '(font-family: JetBrains Mono): status=2152398877 source: http://example.test/font.woff2"]'
      )
    ).toBe(false);
    expect(
      isAbortedFirefoxFontDownload(
        'firefox',
        '[JavaScript Error: "downloadable font: download failed ' +
          '(font-family: JetBrains Mono): status=21523988501 source: http://example.test/font.woff2"]'
      )
    ).toBe(false);
    expect(
      isAbortedFirefoxFontDownload(
        'firefox',
        '[JavaScript Error: "downloadable font: download failed ' +
          '(font-family: JetBrains Mono): status=2152398877 source: http://example.test/font.woff2?status=2152398850"]'
      )
    ).toBe(false);
    expect(
      isAbortedFirefoxFontDownload(
        'chromium',
        '[JavaScript Error: "downloadable font: download failed ' +
          '(font-family: JetBrains Mono): status=2152398850 source: http://example.test/font.woff2"]'
      )
    ).toBe(false);
    expect(
      isAbortedFirefoxFontDownload(
        'firefox',
        '[JavaScript Error: "script download failed: status=2152398850"]'
      )
    ).toBe(false);
  });
});
