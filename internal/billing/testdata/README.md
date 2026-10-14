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
