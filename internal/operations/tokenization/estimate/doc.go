// Package estimate counts the tokens of a request before the provider reports
// them, which is what admission reserves, what the planner compares with a
// model's context window, and what budgets charge. An estimate is neither a
// native count result, which a provider returns for a counting operation, nor
// billed usage.
//
// # Counting
//
// ForModel resolves an upstream model name to a Counter. The models of the
// OpenAI families whose tokenizers OpenAI publishes, o200k_base (GPT-4o,
// GPT-4.1, GPT-5, the o-series) and cl100k_base (GPT-4, GPT-3.5 Turbo,
// text-embedding-3), are counted exactly by an in-repository byte-pair
// encoder. There are two exceptions. One is a single unbroken piece of more than
// 256 KiB, which the encoder merges in chunks and so counts approximately, and
// which a Meter, reading at most ExactBytes and a window past them, never meets.
// The other is a character assigned in a Unicode version newer than the one
// tiktoken's pattern engine reads: the scanners take their letter, number and
// space classes from Go's unicode tables, so a piece holding such a character
// can split differently, and cost a token more or less, than it does there. The
// oracle fixtures hold no such character, and a Go upgrade moves the tables. Every
// other model, including every name the registry does not recognize, uses the
// heuristic of four characters per token times a per-family factor, which is 1
// until the reference catalog supplies measured ones. No name defaults to an
// OpenAI encoding, because a wrong tokenizer miscounts without saying so.
//
// A Counter reports how a count was made as a Provenance. ProvenanceTokenizer
// is exact. ProvenanceHeuristic is the plain four-characters rule.
// ProvenanceCalibrated is a count scaled by a ratio: a heuristic with a family
// factor, or the part of a long prompt past ExactBytes, which a Meter charges
// at the token-to-byte ratio measured on the exact part. A ratio measured on a
// few bytes is not trusted, so a tail with less than a sample's worth of exact
// text before it is charged at four bytes to a token and the count is a
// heuristic. A request's text goes through one Meter per counter, segment by
// segment, so the bound applies to the request and not to each field.
//
// Framing carries the documented per-message overhead of OpenAI's chat format
// for the caller that walks a request's messages.
//
// # Walking a request
//
// Walk reads a request once, in any dialect, into a Prompt: its text as
// segments, its images and media parts as flat charges (ImageTokens and
// MediaTokens), and the roles and message counts a tokenizer's framing is
// charged on. The request is read from its parsed document, where each value is
// found by its place and each string lies in the document's own bytes, so a
// prompt of 400 KB costs the one pass that decodes its escapes and not a scan for
// each level of the request it is nested in. A Prompt is counted on demand for a Counter, by Prompt.Estimate,
// and each family is counted once however many models of it ask, so a route
// whose targets share a family, or a request that is translated for several, is
// priced without reading its text again. Estimate adds the reply the request
// allows, and what a provider's defaults supply, to give what admission
// reserves, what the planner weighs a context window by, and what an attempt
// records. For a family without a tokenizer it is the estimate the gateway
// charged before any family had one, bit for bit, for every shape the old walker
// read; the legacy copy of the old walker in walk_legacy_test.go holds it to that
// over generated requests, and walk_reference_test.go keeps the walker as it read
// raw JSON, which the walker that reads the parsed document is held to, field for
// field, for any request and any provider defaults. The walker reads a few shapes the old one did not: the
// schema of a structured output in each dialect, a Responses reasoning summary,
// Anthropic thinking and document blocks, and Bedrock's tool catalogue.
//
// A tokenizer count is exact only for the text. The flat charge for an image or
// a document is a guess, and a model reads a tool schema, a tool call, the
// schema of a structured output and its own reasoning in a rendering of its own
// that no provider documents, so a request that has one is calibrated, not
// tokenizer, and the framing of the other dialects, which no provider
// documents, follows what each calls a message. Heuristic families are not
// framed.
//
// Some members of a prompt are not read as text at all: encrypted and redacted
// reasoning, and the arguments of an Anthropic tool_use or a Gemini
// functionCall and the response of a functionResponse, which are read only where
// they hold text under a key the walker knows. A request that has one
// is marked approximate and so is calibrated, but its count leaves them out:
// the old walker did, and the equality with it above is what keeps the charge of
// a family without a tokenizer from moving until the reference catalog does.
//
// A Prompt holds the text of its request only as far as a counter reads it, the
// first ExactBytes and a window past them, and for the rest how many bytes
// there are: a request lives as long as its upstream answers and a prompt with
// it. The count a Meter makes depends on nothing it does not read, so the
// bound changes none of them, and TestAPromptKeepsOnlyWhatACounterReads holds
// it to that.
//
// # The encoder
//
// Encoding implements tiktoken's algorithm: split the text into pieces with the
// encoding's pattern, then merge each piece's bytes pairwise by lowest rank.
// scan.go transcribes the two patterns as scanners, because they use lookahead
// that Go's regexp lacks. bpe.go merges short pieces by scanning and long ones
// with a heap, so a long unbroken run costs O(n log n) rather than O(n^2), and
// counting never builds the token list. Special-token text in a prompt is
// ordinary text.
//
// The rank files in ranks/ hold each encoding's token list, in a compact form
// of OpenAI's public file, embedded in the binary and parsed on first use.
// ranks/LICENSE is tiktoken's MIT license, which covers both the algorithm
// transcribed here and the rank data. TestRankSources rebuilds each public file
// from the compact one and checks the SHA-256 that tiktoken pins for it, which
// is also the hash of the files fetched from OpenAI's public blob store on
// 2026-10-01:
//
//	o200k_base   446a9538cb6c348e3516120d7c08b09f57c36495e2acfffe59a5bf8b0cfb1a2d
//	cl100k_base  223921b76ee99bde995b7ff738513eef100fb51d18c93597a113bcffe865b2a7
//
// To regenerate them, run the test with -update-ranks, which downloads the
// public files, checks them against those hashes and rewrites ranks/:
//
//	go test ./internal/operations/tokenization/estimate -run TestUpdateRanks -update-ranks
//
// # Verification
//
// tests/fixtures/tokens holds fixtures generated by OpenAI's tiktoken
// (generate.py there pins the version and says how to rerun it) and
// TestOracleFixtures requires every token id to match, for both encodings. The
// scanners are also checked against a second implementation of the patterns on
// top of regexp, and the heap merge against the textbook merge.
package estimate
