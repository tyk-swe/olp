# Owned Gemini background stream recovery

`TestGeminiBackgroundStreamResumesSameOwnedWorkAfterReaderLoss` qualifies one
strict, versioned Gemini Interactions resource across an accepted background
POST, an SSE GET through a fresh gateway instance, early client reader loss,
and a cursor-resumed SSE GET through another fresh gateway instance. Both GETs
resolve the same encrypted owner mapping and the same upstream resource; no
second provider POST occurs. The client sees ordered native cursor events and
its local resource ID, never the upstream ID. The fixture is an independent
scripted local provider reached through the public management-configured
gateway and the existing PostgreSQL resource authority.

This is a scoped native Interactions contract. It does not claim OpenAI
Responses background streaming, a Live session-resumption handle, a gateway
process crash during upstream acceptance, provider-side exactly-once work, or
live-model quality. Closing the first HTTP reader does not prove that the
provider stopped work; the subsequent cursor read reconstructs the already
accepted resource instead of submitting another interaction.

The selected public integration test passed with race detection on the
isolated implementation branch. Final merged-branch service/SDK and G1–G7
qualification remain separate.
