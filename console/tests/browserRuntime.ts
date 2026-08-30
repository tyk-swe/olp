const firefoxBindingAbortedStatus = String(0x804b0002);

export function isAbortedFirefoxFontDownload(
  browserName: string,
  text: string
): boolean {
  const status = text.match(/: status=(\d+) source: /)?.[1];
  return (
    browserName === 'firefox' &&
    text.startsWith('[JavaScript Error: "downloadable font: download failed') &&
    status === firefoxBindingAbortedStatus
  );
}
