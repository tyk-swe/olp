These minimal native REST fixtures pin `retained-resources-1` codecs and
`openai-evals-rest-fixtures-1`, reviewed on 2026-10-23. They use synthetic IDs,
models and content. They do not establish live-provider availability.

- [OpenAI fine-tuning jobs](https://developers.openai.com/api/reference/resources/fine_tuning/subresources/jobs/methods/retrieve): terminal `trained_tokens` and result-file references.
- [OpenAI eval runs](https://developers.openai.com/api/reference/resources/evals/subresources/runs/methods/retrieve): terminal `per_model_usage`, including cancelled work.
- [OpenAI eval definitions](https://developers.openai.com/api/reference/resources/evals/methods/retrieve) and [output items](https://developers.openai.com/api/reference/resources/evals/subresources/runs/subresources/output_items/methods/retrieve): immutable grader models, diagnostic grader IDs and nested run ownership.
- [Vertex tuning jobs](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/reference/rest/v1/projects.locations.tuningJobs): native timestamps, tuned models and billable training tokens.
- [Gemini caching](https://ai.google.dev/api/caching): immutable model/token quantity and authoritative expiration.

The unchanged OpenAI SDK lock is v3.78.0. That SDK no longer contains the Evals
client; the retained Evals REST codec uses these explicit native fixtures.
SDK-service and live-provider qualification remain separate requirements.
