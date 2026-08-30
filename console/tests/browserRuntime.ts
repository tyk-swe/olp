const firefoxBindingAbortedStatus = `status=${0x804b0002}`;

export function isAbortedFirefoxFontDownload(text: string): boolean {
  return (
    text.startsWith('[JavaScript Error: "downloadable font: download failed') &&
    text.includes(firefoxBindingAbortedStatus)
  );
}
