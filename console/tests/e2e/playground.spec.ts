import { askPlayground, expect, test } from './journey';

for (const streamed of [false, true]) {
  test(`the playground shows the ${streamed ? 'streamed' : 'unary'} answer and what it cost in tokens`, async ({
    page,
    vendor
  }) => {
    const result = await askPlayground(page, streamed);

    await expect(result).toContainText(vendor.answer);
    await expect(
      result.getByText('Total tokens', { exact: true }).locator('..')
    ).toContainText(String(vendor.inputTokens + vendor.outputTokens));
  });
}
