# Immutable media source and staged byte identity

Native image-generation and speech decoders now retain the complete immutable
OIF JSON document alongside their existing typed request fields. The document
keeps original member order, exact numbers, explicit null and the caller's
source bytes; configured defaults still use the established detached field
copies. Multipart requests continue to own staged file Parts rather than
inventing a JSON document for binary input.

The existing private bounded spool computes SHA-256 as it writes each accepted
chunk. A committed artifact, a later `Open` of the same live owner, and the
multipart Part all carry the same lowercase digest, byte length and opaque
handle. `BlobReference` uses those values under the existing spool authority.
If the caller omitted Content-Type, the Part preserves that omission; the blob
reference uses `application/octet-stream` only as a neutral storage type. It
does not add a native modality control or authorize URL fetching. Failed,
partial, oversized and cancelled writes never publish a digest-bearing
artifact; cleanup and capacity release retain their existing owners.

The spool remains private to one process. Its crash cleanup removes abandoned
artifacts while another live spool owner keeps its own bytes and digest. This
foundation identifies bytes already owned by the spool, but does not make
spool files durable across a process restart or provide a new public asset
store. #216 still must integrate strict media request/result/event OIF plans,
resource lifetimes, provider-native references and delivery semantics.

Validation: the complete media package passed with race detection in 4.196 s,
including spool, multipart, recovery, source and slow-peer controls. The public
scripted image/media management and nested-source suite passed with race
detection in 10.582 s. Integration-tagged Go vet and current inventory
generation passed. All providers are scripted locally; no live model or
empirical quality claim is made.
