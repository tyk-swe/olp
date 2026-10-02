#!/usr/bin/env python3
"""Generates the tokenizer oracle fixtures from OpenAI's reference tiktoken.

Run from the repository root:

    uv sync --project tests/fixtures/tokens --frozen
    uv run --project tests/fixtures/tokens --frozen python tests/fixtures/tokens/generate.py

The script works offline and its output is deterministic, so a clean rerun
leaves no diff. tiktoken is pinned in pyproject.toml and uv.lock. It reads the
rank bytes embedded in the Go estimator (internal/operations/tokenization/
estimate/ranks), rebuilds the public .tiktoken text from them, checks it against
the SHA-256 that tiktoken itself pins for the encoding, and serves it to
tiktoken through TIKTOKEN_CACHE_DIR, so the oracle and the estimator tokenize
with the same ranks and any network fetch is an error.

Output, next to this script:

    o200k_base.json, cl100k_base.json   [{name, encoding, text, tokens}]
    framing.json                        chat-message counts by the OpenAI
                                        cookbook formula on top of tiktoken

Texts are encoded with encode_ordinary: special tokens in a prompt are ordinary
text to the estimator, whereas plain encode refuses them.
"""
import base64
import hashlib
import inspect
import json
import os
import pathlib
import random
import tempfile
import unicodedata

HERE = pathlib.Path(__file__).resolve().parent
RANKS = HERE.parents[2] / "internal/operations/tokenization/estimate/ranks"
MAGIC = b"OLPRANK1"
BLOB = "https://openaipublic.blob.core.windows.net/encodings/"
ENCODINGS = ["o200k_base", "cl100k_base"]


def uvarint(data, at):
    value = shift = 0
    while True:
        byte = data[at]
        at += 1
        value |= (byte & 0x7F) << shift
        if byte < 0x80:
            return value, at
        shift += 7


def public_ranks(name):
    """Rebuilds the public .tiktoken text from the embedded compact ranks."""
    data = (RANKS / f"{name}.bin").read_bytes()
    assert data.startswith(MAGIC), name
    count, at = uvarint(data, len(MAGIC))
    lengths = []
    for _ in range(count):
        length, at = uvarint(data, at)
        lengths.append(length)
    lines = []
    for rank, length in enumerate(lengths):
        token = data[at : at + length]
        at += length
        lines.append(base64.b64encode(token) + b" %d\n" % rank)
    assert at == len(data), name
    return b"".join(lines)


def stage_ranks(cache):
    """Puts the embedded ranks where tiktoken looks before it would download."""
    import tiktoken.load
    import tiktoken_ext.openai_public as public

    def no_network(blobpath):
        raise AssertionError(f"tiktoken tried to fetch {blobpath}")

    tiktoken.load.read_file = no_network
    for name in ENCODINGS:
        source = inspect.getsource(getattr(public, name))
        text = public_ranks(name)
        digest = hashlib.sha256(text).hexdigest()
        assert f'"{BLOB}{name}.tiktoken"' in source, name
        assert f'expected_hash="{digest}"' in source, f"{name}: embedded ranks differ from tiktoken's"
        key = hashlib.sha1(f"{BLOB}{name}.tiktoken".encode()).hexdigest()
        (cache / key).write_bytes(text)


# Texts. Everything here is a literal or derived from the seeded generator
# below, so a rerun reproduces the files exactly.

PROSE = [
    "The quick brown fox jumps over the lazy dog.",
    "It was the best of times, it was the worst of times, it was the age of wisdom, it was the age of foolishness.",
    "Call me Ishmael. Some years ago--never mind how long precisely--having little or no money in my purse, "
    "and nothing particular to interest me on shore, I thought I would sail about a little and see the watery part of the world.",
    "Tokenization isn't a solved problem: \"quotes\", (parentheses), [brackets], {braces}; colons: semicolons; "
    "ellipses... and em-dashes -- all of them -- matter!",
    "Dear Sir or Madam,\n\nI am writing to enquire about the position advertised on your website.\n\n"
    "Yours faithfully,\nA. N. Other\n",
    "   Leading spaces, trailing spaces   ",
    "Numbers like 3.14159, 1,000,000, 2^10 = 1024, and 0x7fffffff appear in prose; so do dates (2026-09-30) and times (23:59:59).",
]

CONTRACTIONS = [
    "I'm sure he'll say it's fine, but we've seen they'd rather not; you're right, can't win, won't lose.",
    "I'M SURE HE'LL SAY IT'S FINE, BUT WE'VE SEEN THEY'D RATHER NOT; YOU'RE RIGHT, CAN'T WIN.",
    "Mixed-case: DoN'T, wE'Ll, I'Ve, SHE'D, THEY'RE, it'S, he'LL, o'clock, rock'n'roll, y'all've.",
    "'tis the season; 'twas brillig; 'em all; '90s kids; ''double'' and 'single' quotes.",
    "Typographic apostrophes do not contract: can\u2019t, it\u2019s, I\u2019m, we\u2019ve.",
    "Long s folds to s in the case-insensitive suffix: it'\u017f and it'\u017F, HE'\u017f.",
    "Contraction after digits and marks: 5's, caf\u00e9's, na\u00efve'd, \u4e2d\u6587's, A\u0301's.",
    "'s 't 're 've 'm 'll 'd 'S 'T 'RE 'VE 'M 'LL 'D 'x 'ee 'lx 'rx",
]

CODE_GO = '''package main

import (
\t"context"
\t"fmt"
\t"net/http"
)

// Server answers health checks and shuts down when ctx is cancelled.
type Server struct {
\taddr string
\tmux  *http.ServeMux
}

func (s *Server) Run(ctx context.Context) error {
\tsrv := &http.Server{Addr: s.addr, Handler: s.mux}
\tgo func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
\tif err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
\t\treturn fmt.Errorf("serve %s: %w", s.addr, err)
\t}
\treturn nil
}
'''

CODE_PYTHON = '''import asyncio
from dataclasses import dataclass, field


@dataclass(frozen=True)
class Job:
    name: str
    retries: int = 3
    tags: list[str] = field(default_factory=list)


async def run(jobs: list[Job], *, limit: int = 8) -> dict[str, float]:
    sem = asyncio.Semaphore(limit)
    results: dict[str, float] = {}

    async def one(job: Job) -> None:
        async with sem:
            results[job.name] = await asyncio.sleep(0.01, result=len(job.tags) / 2)

    await asyncio.gather(*(one(j) for j in jobs))
    return results
'''

CODE_JS = '''export async function* paginate(fetchPage, { pageSize = 50, signal } = {}) {
  let cursor = null;
  do {
    const res = await fetchPage({ cursor, limit: pageSize }, { signal });
    if (!res.ok) throw new Error(`page failed: ${res.status} ${res.statusText}`);
    const { items, next_cursor: next } = await res.json();
    yield* items.map((item) => ({ ...item, fetchedAt: Date.now() }));
    cursor = next ?? null;
  } while (cursor !== null);
}

const total = [1, 2, 3].reduce((a, b) => a + b ** 2, 0) / 3.0; // 4.666...
'''

JSON_DOC = json.dumps(
    {
        "id": "chatcmpl-9a8b7c6d5e4f",
        "object": "chat.completion",
        "created": 1750000000,
        "model": "gpt-4o-2024-08-06",
        "choices": [
            {
                "index": 0,
                "message": {"role": "assistant", "content": "Hello! How can I help you today?", "refusal": None},
                "logprobs": None,
                "finish_reason": "stop",
            }
        ],
        "usage": {"prompt_tokens": 19, "completion_tokens": 10, "total_tokens": 29},
    },
    indent=2,
)

TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "get_current_weather",
            "description": "Get the current weather in a given location",
            "parameters": {
                "type": "object",
                "properties": {
                    "location": {"type": "string", "description": "The city and state, e.g. San Francisco, CA"},
                    "unit": {"type": "string", "enum": ["celsius", "fahrenheit"]},
                },
                "required": ["location"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "search_documents",
            "description": "Search the knowledge base; returns at most `limit` hits ranked by BM25.",
            "strict": True,
            "parameters": {
                "type": "object",
                "properties": {
                    "query": {"type": "string"},
                    "limit": {"type": "integer", "minimum": 1, "maximum": 50, "default": 10},
                    "filters": {
                        "type": "object",
                        "additionalProperties": {"anyOf": [{"type": "string"}, {"type": "number"}]},
                    },
                },
                "required": ["query", "limit", "filters"],
                "additionalProperties": False,
            },
        },
    },
]

MULTILINGUAL = {
    "chinese": "\u4eca\u5929\u5929\u6c14\u5f88\u597d\uff0c\u6211\u4eec\u4e00\u8d77\u53bb\u516c\u56ed\u6563\u6b65\u5427\u3002\u4eba\u5de5\u667a\u80fd\u6b63\u5728\u6539\u53d8\u4e16\u754c\u3002",
    "japanese": "\u543e\u8f29\u306f\u732b\u3067\u3042\u308b\u3002\u540d\u524d\u306f\u307e\u3060\u7121\u3044\u3002\u30ab\u30bf\u30ab\u30ca\u3068\u3072\u3089\u304c\u306a\u3068\u6f22\u5b57\u3002",
    "korean": "\ub300\ud55c\ubbfc\uad6d\uc740 \ubbfc\uc8fc\uacf5\ud654\uad6d\uc774\ub2e4. \uc548\ub155\ud558\uc138\uc694, \uc138\uacc4!",
    "arabic": "\u0645\u0631\u062d\u0628\u0627 \u0628\u0627\u0644\u0639\u0627\u0644\u0645\u060c \u0647\u0630\u0627 \u0627\u062e\u062a\u0628\u0627\u0631 \u0644\u0644\u062a\u0631\u0645\u064a\u0632 \u0661\u0662\u0663",
    "hebrew": "\u05e9\u05dc\u05d5\u05dd \u05e2\u05d5\u05dc\u05dd, \u05d6\u05d4\u05d5 \u05de\u05d1\u05d7\u05df",
    "hindi": "\u0928\u092e\u0938\u094d\u0924\u0947 \u0926\u0941\u0928\u093f\u092f\u093e, \u092f\u0939 \u090f\u0915 \u092a\u0930\u0940\u0915\u094d\u0937\u0923 \u0939\u0948\u0964 \u0915\u094d\u0937\u0924\u094d\u0930\u093f\u092f \u0936\u094d\u0930\u0940",
    "thai": "\u0e2a\u0e27\u0e31\u0e2a\u0e14\u0e35\u0e0a\u0e32\u0e27\u0e42\u0e25\u0e01 \u0e19\u0e35\u0e48\u0e04\u0e37\u0e2d\u0e01\u0e32\u0e23\u0e17\u0e14\u0e2a\u0e2d\u0e1a",
    "russian": "\u041f\u0440\u0438\u0432\u0435\u0442, \u043c\u0438\u0440! \u042d\u0442\u043e \u0442\u0435\u0441\u0442 \u0442\u043e\u043a\u0435\u043d\u0438\u0437\u0430\u0442\u043e\u0440\u0430.",
    "greek": "\u0393\u03b5\u03b9\u03ac \u03c3\u03bf\u03c5 \u03ba\u03cc\u03c3\u03bc\u03b5, \u03b1\u03c5\u03c4\u03cc \u03b5\u03af\u03bd\u03b1\u03b9 \u03ad\u03bd\u03b1 \u03c4\u03b5\u03c3\u03c4.",
    "emoji": "\U0001f600 \U0001f680 \U0001f44d\U0001f3fd \U0001f1fa\U0001f1f8 \U0001f1ef\U0001f1f5 \u2764\ufe0f \u2705 \u2728\u2728\u2728",
    "zwj-sequences": "\U0001f468\u200d\U0001f469\u200d\U0001f467\u200d\U0001f466 \U0001f469\u200d\U0001f4bb \U0001f3f3\ufe0f\u200d\U0001f308 \U0001f9d1\U0001f3fe\u200d\U0001f91d\u200d\U0001f9d1\U0001f3fb",
    "combining-marks": "e\u0301 a\u0300\u0301\u0302 n\u0303 Z\u0351\u036b\u0343\u036a\u0302\u036b\u033d\u034f\u0334\u0319\u0324\u031e\u0349\u035a\u032f\u031e\u0320\u034dA\u036b\u0357\u0334\u0362\u0335\u031c\u0330\u0354L\u0368\u0367\u0369\u0358\u0320G\u0311\u0357\u030e\u0305\u035b\u0341\u0334\u033b\u0348\u034d\u0354\u0339O\u0342\u030c\u030c\u0358\u0328\u033b\u033a\u0345!",
    "mixed-scripts": "Hello \u4e16\u754c, \u041f\u0440\u0438\u0432\u0435\u0442, \u0645\u0631\u062d\u0628\u0627, \u0928\u092e\u0938\u094d\u0924\u0947, \u3053\u3093\u306b\u3061\u306f, \uc548\ub155! caf\u00e9 na\u00efve Z\u00fcrich \u00c5ngstr\u00f6m",
    "fullwidth": "\uff28\uff45\uff4c\uff4c\uff4f\u3000\uff37\uff4f\uff52\uff4c\uff44\uff01\u3000\uff11\uff12\uff13\uff14",
    "math-alphanumerics": "\U0001d407\U0001d41e\U0001d425\U0001d425\U0001d428 \U0001d4d0\U0001d4d1\U0001d4d2 \U0001d7d8\U0001d7d9\U0001d7da",
    "supplementary-letters": "\U00010348\U00010349 \U00010400\U00010428 \U00013000\U00013001\U00013002 \U00020000\U00020001 \U0002a6df",
    "unassigned-and-private-use": "\u0378 \ue000\ue001 \U000f0000 \U0010fffd \ufffe\uffff \ufdd0",
}

IDENTIFIERS = [
    "camelCase PascalCase snake_case SCREAMING_SNAKE_CASE kebab-case dot.case Train-Case",
    "HTTPServerError XMLHttpRequest getHTTPResponseCode parseURLs iPhone McDonald eBay LaTeX",
    "ABCdef ABC def ABc ABC1 ABC_def ABC-def ABCdefGHIjkl aBC AbC",
    "utf8string v2beta1 x86_64 sha256sum base64Encode int32_t uint64 H2O CO2 mp3player",
    "__init__ __name__ _private __dunder__ $jquery $$ @decorator #hashtag %percent &ampersand",
    "\u01c4 \u01c5 \u01c6 \u01c8 \u01cb Dž Lj Nj Ǆ Ǉ Ǌ",
    "\u02b0\u02b2\u02e0 hʰ aʲb ʰello Hello\u02b0 \u0e01\u0e32 \u0915\u093e\u0915",
]

DIGITS = [
    "1234567890",
    "0.000123 1,234,567.89 3.14159265358979 -42 +7 1e-9 6.02E23 0xDEADBEEF 0b1010 0o777",
    "1" * 100,
    "".join(str(i % 10) for i in range(1000)),
    "\u0660\u0661\u0662\u0663\u0664 \uff11\uff12\uff13\uff14\uff15 \u00b2\u00b3\u00b9 \u2167 \u2460\u2461 \u00bd\u00be \u0967\u0968\u0969",
    "Call 555-0100 or +1 (415) 555-2671; PIN 0042; room 101.",
    " 1 22 333 4444 55555 666666",
]

WHITESPACE = [
    " ", "  ", "\n", "\n\n", "\t", "\r\n", "\r", " \n", "\n ",
    "a  b   c    d     e",
    "a\n\nb\n\n\nc",
    "line1\r\nline2\r\n\r\nline3",
    "tab\there\t\tthere\t\t\tend",
    "x \n y \t\n z \r\n w",
    "trailing spaces   \n",
    "trailing newline\n",
    "trailing blank lines\n\n\n",
    "trailing run  \n  ",
    "trailing tab \t",
    "   leading run",
    "\n\n   leading after newlines",
    "word \u00a0nbsp\u00a0 \u3000ideographic\u3000 \u2003em\u2003space \u2028line\u2028sep \u2029para \u0085nel\u0085",
    "zero\u200bwidth\u200cnon\u200djoiner \ufeffbom \u2060word-joiner",
    "mix \t \n  \r\n   \t\t\n\n x",
    " " * 10,
    " " * 100 + "x",
    "x" + " " * 100,
    " " * 10000,
    "\n" * 500,
    "\t" * 300 + "\n" * 3,
    " \n" * 200,
    "    indented\n        more indented\n            deeply indented\n",
    "a \n\u3000\n b",
]

PUNCTUATION = [
    "!!! ??? ... --- ___ === *** ### ~~~ ^^^ +++ <<< >>>",
    "?!?! !?!? .,.,. ;:;: ()()() []{}<>",
    "/**/ // comment /* block */ # shell <!-- html --> -- sql",
    "http://example.com/a/b?c=d&e=f#frag https://x.y/z//w/// user@example.com",
    "C:\\Users\\alice\\Documents\\file.txt /usr/local/bin/../lib ./relative//path",
    "!\n/ ?\r\n// .\n\n/ -/-/-\n",
    "$100.00 \u20ac50 \u00a3\u00a3 \u00a5 100% 5\u00b0C \u00a9 \u00ae \u2122 \u00a7 \u00b6 \u2020 \u2022",
    "```python\nprint('hi')\n```\n\n> quote\n- item\n  - nested\n1. one\n2. two\n",
    "| a | b |\n|---|---|\n| 1 | 2 |\n",
    "\u201cSmart quotes\u201d \u2018single\u2019 \u2013 en \u2014 em \u2026 \u00ab guillemets \u00bb \u300c\u300d",
    "\U0001f4a9" * 50,
    "=" * 400,
    "-" * 400,
    "*" * 4000,
    "/" * 300 + "\n" * 4 + "/" * 300,
]

SPECIAL_TEXT = [
    "<|endoftext|>",
    "Hello<|endoftext|>World",
    "<|im_start|>user\nHi there<|im_end|>\n<|im_start|>assistant\n",
    "<|fim_prefix|>def f():<|fim_suffix|>return 1<|fim_middle|>",
    "<|endofprompt|> <|startoftext|> <|return|> <|channel|>analysis<|message|>",
    "<|endoftext|>" * 20,
    "<| not special |> <|endoftext| <endoftext|>",
]

WORDS = (
    "the of and to in is that for it as was with be by on not he this are or his from at which but have an had they "
    "you were their one all we can her has there been if more when will would who so no what up out about into than "
    "them could other only new some time these two may first then do any like my now over such our man me even most "
    "made after also did many before must through back years where much your way well down should because each just "
    "those people how too little state good very make world still own see men work long get here between both life "
    "being under never day same another know while last might us great old year off come since against go came right "
    "used take three tokenizer admission budget provider gateway upstream latency throughput streaming completion "
    "encoding vocabulary prompt estimate calibration heuristic reservation concurrency idempotency observability"
).split()


def document(rng, size):
    """A long English-like document with paragraphs, lists and the odd number."""
    out = []
    length = 0
    while length < size:
        sentence = []
        for i in range(rng.randrange(6, 24)):
            word = rng.choice(WORDS)
            if i == 0:
                word = word.capitalize()
            elif rng.random() < 0.03:
                word = word.upper()
            elif rng.random() < 0.02:
                word = str(rng.randrange(10**rng.randrange(1, 7)))
            sentence.append(word)
            if rng.random() < 0.08 and i < 20:
                sentence[-1] += rng.choice([",", ";", ":"])
        text = " ".join(sentence) + rng.choice([".", ".", ".", "!", "?"])
        roll = rng.random()
        if roll < 0.12:
            text += "\n\n"
        elif roll < 0.16:
            text += "\n- "
        else:
            text += " "
        out.append(text)
        length += len(text)
    return "".join(out)


def base64_text(rng, size):
    return base64.b64encode(bytes(rng.randrange(256) for _ in range(size))).decode()


# The fuzz set mixes these classes. Each pool is a list of snippets, and random
# code points come from every plane, skipping what Python's Unicode tables
# leave unassigned.
POOLS = {
    "word": WORDS,
    "ident": ["camelCase", "PascalCase", "snake_case", "HTTPServer", "ABCdef", "XMLParser", "getID", "iOS", "A", "Z"],
    "contraction": ["'s", "'t", "'re", "'ve", "'m", "'ll", "'d", "'S", "'LL", "'\u017f", "\u2019s", "'x"],
    "digits": ["0", "7", "42", "123", "1234", "98765", "000", "3.14", "\u0663\u0664", "\uff15", "\u00b2"],
    "punct": ["!", "?", "...", "--", "==", "/", "//", "\\", "{", "}", "(", ")", "[", "]", ",", ".", ":", ";", "@", "#", "$", "%", "&", "*", "+", "<", ">", "|", "~", "^", "`", '"', "'"],
    "space": [" ", "  ", "   ", "\t", "\n", "\n\n", "\r\n", " \n", "\n ", "\u00a0", "\u3000", "\u2003", "\u2028", "\u0085", "        "],
    "cjk": ["\u4e2d\u6587", "\u6c49\u5b57", "\u65e5\u672c\u8a9e", "\u3072\u3089\u304c\u306a", "\u30ab\u30bf\u30ab\u30ca", "\ud55c\uae00", "\u4eca\u5929"],
    "rtl": ["\u0645\u0631\u062d\u0628\u0627", "\u05e9\u05dc\u05d5\u05dd", "\u0627\u0644\u0639\u0631\u0628\u064a\u0629"],
    "indic": ["\u0928\u092e\u0938\u094d\u0924\u0947", "\u0915\u094d\u0937", "\u0e2a\u0e27\u0e31\u0e2a\u0e14\u0e35", "\u0e01\u0e32"],
    "emoji": ["\U0001f600", "\U0001f680", "\U0001f44d\U0001f3fd", "\U0001f468\u200d\U0001f469\u200d\U0001f467", "\U0001f1fa\U0001f1f8", "\u2764\ufe0f", "\u2728"],
    "marks": ["\u0301", "\u0300\u0301", "\u0303", "\u0951", "\u200d", "\u200c", "\ufe0f", "\u0e31", "\u093e"],
    "accented": ["caf\u00e9", "\u00c9COLE", "na\u00efve", "\u00dcber", "\u00e0\u00e8\u00ec", "\u01c5", "\u02b0"],
}
POOL_NAMES = sorted(POOLS)
RANGES = [(0x20, 0x7E, 6), (0xA0, 0x24F, 3), (0x250, 0x2FF, 1), (0x300, 0x36F, 1), (0x370, 0x58F, 2), (0x590, 0x6FF, 2),
          (0x900, 0xDFF, 2), (0xE00, 0x1FFF, 2), (0x2000, 0x2BFF, 2), (0x2C00, 0x2FFF, 1), (0x3000, 0x9FFF, 3),
          (0xA000, 0xD7FF, 1), (0xE000, 0xFFFD, 1), (0x10000, 0x1FFFF, 3), (0x20000, 0x2FFFF, 2), (0xE0000, 0xE0FFF, 1)]


def random_codepoint(rng):
    while True:
        lo, hi, _ = rng.choices(RANGES, weights=[r[2] for r in RANGES])[0]
        cp = rng.randrange(lo, hi + 1)
        if unicodedata.category(chr(cp)) not in ("Cn", "Cs"):
            return chr(cp)


def fuzz_string(rng):
    parts = []
    for _ in range(rng.randrange(1, 40)):
        roll = rng.random()
        if roll < 0.1:
            parts.append("".join(random_codepoint(rng) for _ in range(rng.randrange(1, 6))))
        elif roll < 0.15:
            parts.append(rng.choice("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") * rng.randrange(1, 60))
        else:
            parts.append(rng.choice(POOLS[rng.choice(POOL_NAMES)]))
        # Words usually follow a space, as in prose, but not always.
        if rng.random() < 0.55:
            parts.append(" ")
    return "".join(parts)


def encode(encodings, name, text):
    return encodings[name].encode_ordinary(text)


def write_json(path, fixtures):
    lines = [json.dumps(f, ensure_ascii=True, separators=(",", ":")) for f in fixtures]
    path.write_text("[\n" + ",\n".join(lines) + "\n]\n", newline="\n")


def build_texts():
    rng = random.Random(20260930)
    texts = []

    def add(name, text):
        texts.append((name, text))

    add("empty", "")
    for i, t in enumerate(PROSE):
        add(f"prose-{i}", t)
    for i, t in enumerate(CONTRACTIONS):
        add(f"contractions-{i}", t)
    add("code-go", CODE_GO)
    add("code-python", CODE_PYTHON)
    add("code-javascript", CODE_JS)
    add("json-pretty", JSON_DOC)
    add("json-compact", json.dumps(json.loads(JSON_DOC), separators=(",", ":")))
    add("tools-pretty", json.dumps(TOOLS, indent=2))
    add("tools-compact", json.dumps(TOOLS, separators=(",", ":")))
    add("tools-one-line", json.dumps(TOOLS))
    for name, t in MULTILINGUAL.items():
        add(f"multilingual-{name}", t)
    add("multilingual-cjk-long", "\u4eca\u5929\u5929\u6c14\u5f88\u597d\u6211\u4eec\u4e00\u8d77\u53bb\u516c\u56ed\u6563\u6b65" * 300)
    add("multilingual-thai-unbroken", "\u0e2a\u0e27\u0e31\u0e2a\u0e14\u0e35\u0e0a\u0e32\u0e27\u0e42\u0e25\u0e01\u0e19\u0e35\u0e48\u0e04\u0e37\u0e2d\u0e01\u0e32\u0e23\u0e17\u0e14\u0e2a\u0e2d\u0e1a" * 120)
    for i, t in enumerate(IDENTIFIERS):
        add(f"identifiers-{i}", t)
    for i, t in enumerate(DIGITS):
        add(f"digits-{i}", t)
    for i, t in enumerate(WHITESPACE):
        add(f"whitespace-{i}", t)
    for i, t in enumerate(PUNCTUATION):
        add(f"punctuation-{i}", t)
    for i, t in enumerate(SPECIAL_TEXT):
        add(f"special-token-text-{i}", t)
    for name, t in [("a", "a" * 5000), ("ab", "ab" * 3000), ("word", "supercalifragilisticexpialidocious" * 200),
                    ("upper", "ABCDEFGH" * 800), ("mixed-case", "aBcDeF" * 1000)]:
        add(f"long-unbroken-{name}", t)
    add("base64-6k", base64_text(rng, 4500))
    add("base64-data-uri", "data:image/png;base64," + base64_text(rng, 3000) + "==")
    add("hex-dump", "".join(rng.choice("0123456789abcdef") for _ in range(4096)))
    add("document-52k", document(rng, 52 * 1024))
    add("document-markdown-8k", "# Title\n\n" + document(rng, 4096) + "\n\n## Section\n\n" + document(rng, 4096))
    for i in range(320):
        add(f"fuzz-{i:03d}", fuzz_string(random.Random(1_000_003 * (i + 1))))
    return texts


FRAMING = [
    ("single-user-message", [{"role": "user", "content": "hello"}]),
    ("system-and-user", [{"role": "system", "content": "You are a helpful assistant."},
                         {"role": "user", "content": "What is the capital of France?"}]),
    ("multi-turn", [{"role": "system", "content": "Answer in one word."},
                    {"role": "user", "content": "Capital of France?"},
                    {"role": "assistant", "content": "Paris."},
                    {"role": "user", "content": "And of Japan?"}]),
    ("with-names", [{"role": "system", "name": "example_user", "content": "New synergies will help drive top-line growth."},
                    {"role": "system", "name": "example_assistant", "content": "Things working well together will increase revenue."},
                    {"role": "user", "content": "Let's talk later."}]),
    ("empty-content", [{"role": "user", "content": ""}, {"role": "assistant", "content": ""}]),
    ("unicode", [{"role": "user", "content": "\u4eca\u5929\u5929\u6c14\u600e\u4e48\u6837\uff1f \U0001f600 caf\u00e9"}]),
    ("code-and-whitespace", [{"role": "user", "content": CODE_GO}, {"role": "assistant", "content": "  \n\n  done  \n"}]),
    ("tool-message", [{"role": "user", "content": "weather?"},
                      {"role": "tool", "name": "get_current_weather", "content": "{\"temp\":21,\"unit\":\"celsius\"}"}]),
]
FRAMING_MODELS = [("gpt-4o", "o200k_base"), ("gpt-4", "cl100k_base")]


def framing_fixtures(encodings):
    """Counts messages as the cookbook's num_tokens_from_messages does for the
    gpt-3.5-turbo, gpt-4 and gpt-4o families: three tokens per message, the
    value of every field encoded (the role included), one more token for a name,
    and three to prime the reply."""
    out = []
    for model, name in FRAMING_MODELS:
        enc = encodings[name]
        for label, messages in FRAMING:
            tokens = 3
            for message in messages:
                tokens += 3
                for key, value in message.items():
                    tokens += len(enc.encode_ordinary(value))
                    if key == "name":
                        tokens += 1
            out.append({"name": f"{model}-{label}", "model": model, "encoding": name, "messages": messages, "tokens": tokens})
    return out


def main():
    with tempfile.TemporaryDirectory() as cache:
        os.environ["TIKTOKEN_CACHE_DIR"] = cache
        stage_ranks(pathlib.Path(cache))
        import tiktoken

        encodings = {name: tiktoken.get_encoding(name) for name in ENCODINGS}
        texts = build_texts()
        for name in ENCODINGS:
            fixtures = [{"name": n, "encoding": name, "text": t, "tokens": encode(encodings, name, t)} for n, t in texts]
            write_json(HERE / f"{name}.json", fixtures)
            print(f"{name}: {len(fixtures)} fixtures, {sum(len(f['tokens']) for f in fixtures)} tokens")
        write_json(HERE / "framing.json", framing_fixtures(encodings))


if __name__ == "__main__":
    main()
