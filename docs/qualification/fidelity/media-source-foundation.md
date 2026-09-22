# Native JSON media source validation

The existing image-generation and speech JSON decoders retained raw top-level
members for configured defaults, but their interim parser only rejected
duplicate names at the top level. A nested native extension could contain two
names that decode to the same Unicode string, and the standard JSON decoder
could repair malformed Unicode. Accepting either form before dispatch creates
an ambiguous provider invocation.

These two native JSON surfaces now use the bounded immutable OIF source parser
before returning a validated media request. It rejects duplicates at every
nesting depth, including escaped spellings of the same name, invalid UTF-8,
invalid surrogate pairs, excessive nesting/nodes/bytes, and trailing documents.
The compatibility `SourceFields` map consists of detached raw spans from the
validated document. Unit checks confirm that unsafe integers, negative zero,
tiny exponents, explicit null and nested source bytes remain exact. Decoding
still uses the existing media request and profile-default path, and the media quota,
spool, egress, authorization and accounting owners remain in place.

This is an input-source foundation for #216. It does not yet make typed media
requests/results/events authoritative OIF plans, change multipart input
ownership, or advertise new strict media combinations. A public image test
provisions a real provider and route, checks a valid returned image asset, then
proves nested duplicate and malformed Unicode requests
fail before any additional provider dispatch. Focused unit tests cover retained
source ownership and corruption; existing native image/speech positives and
media limit tests remain required controls.

Focused media unit and public image tests passed with race detection; the public
fixture served a valid local image and made no additional provider dispatch on
three ambiguous requests. Go vet and current inventory checks are recorded in
the implementation handoff. Empirical model quality remains unknown; no live
model calls occur.
