# Billing fixtures

These pages follow the documented response shapes of the OpenAI
[organization costs](https://developers.openai.com/api/reference/resources/admin/subresources/organization/subresources/usage/methods/costs/)
endpoint and the Anthropic
[cost report](https://platform.claude.com/docs/en/api/admin/cost_report/retrieve)
endpoint. They are hand-built, not captured from live accounts.

Each source has two pages linked by `has_more` and `next_page`. The pages hold
an empty bucket, which records an invoiced zero for its day, and null line
items, descriptions and cost types, which read as `unspecified`. A null
Anthropic `workspace_id` is the `default` workspace. Amounts such as `0.1`,
`1234.5678901234` and `0.0000000001` would lose precision as floats, and
Anthropic reports cents as strings with a lowercase `usd` currency tag.
