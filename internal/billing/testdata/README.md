# Billing fixtures

Hand-built fixtures in the documented response shapes of:

- **OpenAI** `GET /v1/organization/costs` —
  <https://developers.openai.com/api/reference/resources/admin/subresources/organization/subresources/usage/methods/costs/>
- **Anthropic** `GET /v1/organizations/cost_report` —
  <https://platform.claude.com/docs/en/api/admin/cost_report/retrieve>

These are **documented-shape fixtures, not captures from live accounts**.
They exercise:

- two pages each, with `has_more` / `next_page` pagination;
- an empty bucket (`"results": []`), which records an invoiced zero for the
  day;
- `line_item: null` (OpenAI) and `description: null` / `cost_type: null`
  (Anthropic), which read as `unspecified`, and `workspace_id: null`, which
  is Anthropic's `default` workspace;
- several projects (`proj_...`) and workspaces (`wrkspc_...`);
- amounts whose float parsing would be inexact (`0.1`, `0.2`,
  `1234.5678901234`, `0.0000000001`, `123456.78901234` cents);
- Anthropic cent strings (`"123.45"`, `"0.01"`) and a lowercase `usd`
  currency tag.

Google detailed export fixtures use the documented [detailed export schema](https://docs.cloud.google.com/billing/docs/how-to/export-data-bigquery-tables/detailed-usage) and the documented [Vertex AI billing service identifier](https://docs.cloud.google.com/billing/docs/how-to/budgets-spend-caps), accessed 2026-10-31. The values are invented and include usage in several projects and services, UTC offsets, regular charges, billing modifications and repeated credits. The integration test executes the submitted GoogleSQL using GoogleSQLite, substituting a normal fixture column for BigQuery's `_PARTITIONTIME` pseudo-column. This checks query behavior against the fixtures but does not verify actual BigQuery partition pruning, export completeness or live account behavior.
