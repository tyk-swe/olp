# Strict durable file, batch, and unary background qualification (#216 slice)

Strict `batch` routes now compile a separate native OpenAI-family durable
contract at publication. The admitted profile must be versioned, support the
batch operation and hosting path, have no unimplemented batch defaults or
content mutation policy, and use an explicitly certified per-item operation.
The gateway's existing Attempt, credential slot, quota, media spool, resource
resolver, KeyRing, and accounting owners remain in charge. Legacy file/batch
routes retain their existing resource kind and behavior.

An accepted strict file or batch has its own `strict_file` or `strict_batch`
resource identity. Migration `0029` makes those kinds unreadable as legacy
metadata-only resources. Its ordinary `metadata` is only a bounded serving
index; the original source, effective request, complete latest native result,
serving identity and any uploaded asset SHA-256/size/type are stored under the
same resource UUID by the installation's encrypted secret authority. The
source and result are byte arrays inside the encrypted contract, so a JSON
member's original spacing, order, null and numeric spelling survive storage.
The OIF overlay changes only the owner-scoped input file ID and, for Azure,
the selected model. Returned batch `id`, `input_file_id`, `output_file_id`, and
`error_file_id` are mapped to owner-scoped local IDs without normalizing
unrelated native members. Output and error JSONL files remain separate exact
byte streams; the private spool computes a SHA-256 for each bounded download
before a successful response is committed. All mappings have a maximum local
retention of seven days, further limited by a provider expiry when supplied.

Before uploading a strict JSONL file, the gateway checks at most 10,000
items, each no larger than 1 MiB. `custom_id` is unique and at most 128 bytes;
each item must use `POST`, the same supported endpoint, a body naming the
selected native model, and a provider/route capability certified for that
operation. The batch creation endpoint must equal the uploaded file's
verified endpoint. This does not split or recombine items: the exact file
bytes go to the selected provider, and its partial successes and failures
return from distinct output/error files. The qualified item endpoints are
`/v1/chat/completions`, `/v1/responses`, and `/v1/embeddings` when the selected
profile's native dialect and both capability authorities permit them.
Mixed models/endpoints, duplicate IDs, unsupported item types, caller-supplied
provider-owned result fields, malformed native results, and input-file
identity drift fail before provider dispatch or before result persistence,
as applicable. The gateway does not inspect or reinterpret per-item output
JSONL after retrieval; it returns those native bytes with a digest.

The owner-scoped mapping commits independently of a disconnected client
after a provider returns an accepted ID. A fault test pauses the actual
`provider_resources` INSERT with a PostgreSQL lock, disconnects the client,
then observes one decryptable mapping and one provider submission. A response
before all multipart bytes were sent is an ambiguous accepted-work outcome:
it publishes no completed file ID, records `outcome-unknown` separately from
the unobserved client, and performs no automatic replay. No provider-side
idempotency or exactly-once claim is made. Every strict lookup checks current
key permission, published route, historical provider/slot/credential and
network authority before provider contact. Expired resources disappear;
cleanup removes the associated ciphertext. File and batch lists fail
explicitly if an owned entry cannot be projected, rather than omitting it.
Concurrent first polls converge on one local identity per native output/error
file. Accepted cancellation commits despite a client disconnect; a later
stale provider poll cannot move `cancelling` back to `validating`, and a
repeat cancel returns the committed local state without another provider
mutation. Terminal batches reject another cancellation before dispatch.

Strict native Responses additionally admits **unary** `background:true` only
with retained provider state and an authorized state-enabled key. It records
the encrypted native response resource before returning its local ID, keeps
one content-free pending usage template, and reconciles terminal provider
usage once across repeated GETs. `background:true,store:false` and strict
background streaming are refused before dispatch. Existing native/legacy
background streaming remains a separate compatibility control; it is not
covered by this stronger strict contract. Strict Responses retain their
24-hour resource window.

Public `TestStrictBatchSourcePartialFilesAndLifecycle` publishes the provider
and strict route through management, verifies exact source/effective/result
bytes including `9007199254740993` and `-0`, checks owner and current
credential authority, cancellation, one success and one failure file,
lists, expiry and ciphertext cleanup. The fixture's Azure batch profile is
explicit; official OpenAI-host media/batch certification is not bypassed in
production. `TestStrictBatchSurvivesGatewayRestartWithPartialFiles` restarts
real gateway processes twice and retrieves both file identities without
another batch POST. The pinned first-party OpenAI JavaScript 7.4.0 and Python
3.8.0 clients each perform file upload, batch create, cancel, retrieve and
partial output/error file download through the public gateway with exactly
one provider batch creation. Separate public tests reject uncertified item
operations before upload, prove accepted mapping survives client disconnect,
prove accepted cancellation survives client disconnect, verify eight
concurrent first polls keep one output/error identity, and prove a provider's
early response to partial upload cannot become a successful file.
`TestStrictUnaryBackgroundResponseRetainsOneAcceptedWork`
checks strict queued creation, polling and one terminal usage record.

The final selected strict batch/SDK/disconnect/partial-upload fault cases
passed under `-race` in 19.109 seconds. The fresh-binary process,
strict-background, legacy batch, accepted-disconnect and migration selection
passed under `-race` in 9.336 seconds. A final full `make check` on the
media-composed durable branch passed: generated API, Go vet and unit suites,
console types/lint/647 tests, and 30 script tests.
The final merged branch must rerun these checks. All providers in these tests
are local scripted fixtures; live model quality and frozen G6 performance
remain unmeasured or separately failed in their retained evidence.

This slice does not claim strict video, cloud media wrapper equivalence,
document conversion, arbitrary batch endpoints, cross-provider failover of
accepted work, or resumable strict streaming background. Those combinations
remain outside its admitted contract and the broader #216/#218 gate.
