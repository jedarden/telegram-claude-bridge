# splitParallelPrompts — Test Coverage Summary

## Status: ✅ COMPLETE

Coverage documentation for the `splitParallelPrompts` function, produced under bead
`telegram-fc70833e` (parent: `telegram-f65d8ec6`, "Verify all splitParallelPrompts
tests pass together").

## The contract being tested

**Implementation:** `internal/bridge/commands.go:2085` (`splitParallelPrompts`)

The function splits a user message into parallel subtask prompts. The behavioral
contract the suite pins down:

- **Delimiter** — a line consisting of `---` with only spaces around it, and only
  in input that contains at least one newline. The line-based splitter landed in
  commit `c0484ef` ("consume line delimiters in parallel prompts").
- **Lookalikes are content** — tabs or CRLF adjacent to the dashes (`\t---`, `---\t`),
  `--`, `----`, mid-line `---` (`First---Second`), and Unicode dash lookalikes
  (em/en dashes, horizontal bar, `➖`) never split.
- **Bare `---` is content** — a single-line input of exactly `---` yields one
  prompt, `---`.
- **Empty segments are filtered** — consecutive delimiters and leading/trailing
  delimiters collapse; whitespace-only segments are dropped.
- **Whitespace** — outer whitespace of each segment is trimmed; internal spaces,
  tabs, and newlines are preserved verbatim.
- **No length limit** — the function never truncates. `maxMessageLen` (4096,
  `internal/bridge/sender.go:22`) belongs to the Telegram sender and is applied
  downstream when *results* are posted back, never to prompts on the way in.
  The "length limit" tests below assert the *absence* of truncation; their
  expectations were updated for the current splitter in commits `06f4c7e` and
  `ad8e01d`.
- **No Unicode normalization** — precomposed and decomposed forms of the same
  grapheme come back as the distinct byte sequences they went in as.

## Test inventory

69 test functions across four files, all passing:

| File | Functions | Pattern |
|------|-----------|---------|
| `internal/bridge/subtask_orchestrator_test.go` | 32 | Table-driven |
| `internal/bridge/subtask_orchestrator_basic_test.go` | 5 | Table-driven |
| `internal/bridge/commands_test.go` | 2 | Table-driven |
| `internal/bridge/integration_test.go` | 30 | Single-case direct assertions |

The table-driven tests expand to **390 subtests** (`t.Run(tt.name, …)` per case).

## Coverage by category

### 1. Empty inputs and whitespace

| Test function | File | Covers |
|---|---|---|
| `TestSplitParallelPrompts_EmptyInputs` | orchestrator | empty string, newline-only, spaces-only, tabs-and-spaces, mixed whitespace → 0 prompts |
| `TestSplitParallelPrompts_EmptySegments` | orchestrator | empty/whitespace-only segments between, at start, at end |
| `TestSplitParallelPrompts_WhitespacePreservation` | orchestrator | internal spaces/tabs/newlines/indentation preserved, outer trimmed |
| `TestSplitParallelPrompts_WhitespaceAroundDelimiters` | orchestrator | spaces before/after/around delimiter split; tabs do not; segment-edge trimming |
| `TestSplitParallelPrompts_BasicSplits` | basic | includes empty-string and whitespace-only → no prompts |
| `TestSplitParallelPrompts_WhitespaceContentHandling` | basic | internal whitespace preserved, outer trimmed |
| `TestSplitParallelPrompts_EmptyString` / `_OnlyWhitespace` / `_EmptyPromptsFiltered` / `_WhitespaceOnlyPrompts` / `_TrailingWhitespace` / `_TabsAndMixedWhitespace` / `_WithWhitespace` | integration | single-case variants of the same behaviors |

### 2. Delimiter handling

| Test function | File | Covers |
|---|---|---|
| `TestSplitParallelPrompts_BasicSplits` | basic | 1–5 prompts, spaced delimiter, multi-line prompts, delimiter removal |
| `TestSplitParallelPrompts_SplitCountVerification` | basic | delimiter count ↔ prompt count (0–4 delimiters) |
| `TestSplitParallelPrompts_DelimiterHandling` | basic | standard split; `--`, `----`, no-newline dashes don't split; edges don't create empties |
| `TestSplitParallelPrompts_DelimiterRemoval` | basic | no `\n---\n` remnants in output |
| `TestSplitParallelPrompts_DelimiterAtEdges` | orchestrator | delimiter at start/end/both edges, repeated at edges |
| `TestSplitParallelPrompts_ConsecutiveDelimiters` | orchestrator | 2–4 consecutive delimiters (middle/start/end) collapse to one split point |
| `TestSplitParallelPrompts_MixedDelimiterPatterns` | orchestrator | space-padded delimiter splits; tab-flanked does not; `---` without newlines is content |
| `TestSplitParallelPrompts_NonDelimiterDashes` | orchestrator | `-`, `--`, `----`, `-----`, mid-line `---`, tab-adjacent dashes all stay content |
| `TestSplitParallelPrompts_DelimiterInContent` | orchestrator | markdown lists, `---help` mid-line |
| `TestSplitParallelPrompts_DelimiterPositionEdgeCases` | orchestrator | `---` at true start/end of input; input that is only `---`; delimiter-after-leading-`---` |
| `TestSplitParallelPrompts` / `_ConsumesLineDelimiters` | commands_test | canonical split cases; edge delimiters consumed; bare `---` is content (matches `c0484ef`) |
| `_Basic`, `_DelimiterVariations`, `_DelimiterAtStart`/`_End`, `_MultipleConsecutiveDelimiters`, `_FourDashes`, `_TwoDashes`, `_OnlyDashes`, `_OnlyDelimiter`, `_NewlineOnlyDelimiters`, `_MultipleNewlineOnlyDelimiters`, `_NoDelimiter` | integration | single-case variants |

### 3. Unicode and emoji preservation

| Test function | File | Covers |
|---|---|---|
| `TestSplitParallelPrompts_UnicodeEdgeCases` | orchestrator | emoji throughout, Japanese, Chinese, Arabic (RTL), mixed scripts, Cyrillic, symbols |
| `TestSplitParallelPrompts_UnicodeCharacterPreservation` | orchestrator | byte-exact equality for Latin Extended, Cyrillic, Greek (incl. polytonic), UTF-8 validity, no U+FFFD, **no normalization** (precomposed ≠ decomposed stays distinct) |
| `TestSplitParallelPrompts_UnicodeDelimiterBoundaries` | orchestrator | multibyte runes touching the delimiter; combining marks at seams; NBSP/ideographic space trimmed; em/en-dash and horizontal-bar lookalikes don't split |
| `TestSplitParallelPrompts_CommonEmojiPreservation` | orchestrator | faces, animals, symbols (VS16 and text presentation), objects, emoji-only prompts, byte/rune-exact via `checkEmojiPrompts` |
| `TestSplitParallelPrompts_MultiByteEmojiSequences` | orchestrator | ZWJ sequences (family, professions, handshake), skin-tone modifiers, regional-indicator flags (incl. tag-sequence Scotland/England), keycaps, variation selectors; per-prompt rune counts guard dropped joiners |
| `TestSplitParallelPrompts_EmojiAtDelimiterBoundaries` | orchestrator | emoji/ZWJ/flags/skin-tone/keycap directly against delimiter; emoji-flanked lookalikes don't split; `➖➖➖` is not a delimiter |
| `TestSplitParallelPrompts_UnicodeAtBoundaries` | orchestrator | emoji/CJK/RTL adjacent to delimiter, mixed across three segments |
| `TestSplitParallelPrompts_ZeroWidthCharacters` | orchestrator | U+200B/U+200C/U+200D survive inside text, at boundaries, in long content |
| `TestSplitParallelPrompts_KoreanHangul` | orchestrator | Hangul syllables, Jamo clusters, Korean+ASCII, Korean+emoji, long Korean text |
| `TestSplitParallelPrompts_AdditionalCombiningMarks` | orchestrator | macron, breve, diaeresis, cedilla, ogonek, ring, tilde, caron, double acute, horn, hook above; stacked marks |
| `TestSplitParallelPrompts_MixedUnicodeASCII` | orchestrator | ASCII+accents/currency/math, embedded emoji, 2–4 scripts per prompt, RTL+LTR, patterns across boundaries |
| `_UnicodeContent` / `_UnicodeCharacters` | integration | single-case Unicode preservation |

### 4. Length limits and boundaries

These pin the **no-truncation** contract (see "The contract being tested" above).
Exact-equality checks come from the `checkNoTruncation` helper (byte count, rune
count, first-divergence offset, UTF-8 validity, no U+FFFD).

| Test function | File | Covers |
|---|---|---|
| `TestSplitParallelPrompts_LongInputs` | orchestrator | 10 KB single prompt, 2×5 KB prompts, five 300-char prompts |
| `TestSplitParallelPrompts_VeryLongSinglePrompt` | orchestrator | single prompts from 100 to 1,000,000 chars, no delimiter |
| `TestSplitParallelPrompts_ByteBoundaryLengths` | orchestrator | exact 256/512/1024/**4096 (maxMessageLen)**/8192-byte prompts pass through intact — the sender splits at 4096, the splitter must not pre-truncate |
| `TestSplitParallelPrompts_SinglePromptExceedsLengthLimit` | orchestrator | maxMessageLen−1 / exactly / +1 / ×10, sentinel tail to detect prefix cuts, whitespace-wrap trims edges only |
| `TestSplitParallelPrompts_MultipleLongPromptsNotTruncated` | orchestrator | every segment of multi-prompt input at full length, total far over the limit |
| `TestSplitParallelPrompts_UnicodeExceedsLengthLimit` | orchestrator | oversized CJK/emoji/combining/mixed content — a byte cut at 4096 would land mid-rune; checks for severed first/last runes |
| `TestSplitParallelPrompts_LimitBoundaryAtDelimiterVsMidPrompt` | orchestrator | maxMessageLen landing on delimiter start/end/inside, mid-prompt, last byte, mid-rune; literal `---` at the limit stays content |
| `TestSplitParallelPrompts_ManyDelimitersInLongInput` | orchestrator | 10×1 KB segments, 3 oversized segments, collapsed consecutive delimiters, indented `---` deep in long input, `-----` rule doesn't split, 50×200-char prompts |
| `TestSplitParallelPrompts_LengthLimitBoundaries` | orchestrator | extreme *short* end: 0–10 byte prompts, 1–3 byte Unicode, ZWJ/skin-tone/flag/keycap byte sizes, short prompts at delimiter boundaries |
| `_LongPrompts` / `_LongPrompt` | integration | single-case long-input variants |

### 5. Special characters

| Test function | File | Covers |
|---|---|---|
| `TestSplitParallelPrompts_SpecialCharsEdgeCases` | orchestrator | backticks/code spans, shell `$PATH`/`${HOME}`, markdown `**bold**`/`_underline_`, nested JSON, quotes and escapes |
| `_SpecialCharacters` | integration | single-case variant |

### 6. Edge cases

| Test function | File | Covers |
|---|---|---|
| `TestSplitParallelPrompts_MaxPrompts` | orchestrator | 6, 10, and 20 prompts — no prompt-count ceiling |
| `TestSplitParallelPrompts_MixedNewlines`, `_TabCharacters`, `_ExactlyFivePrompts`, `_SixPromptsBoundary`, `_FivePrompts`, `_MultiLinePrompts` | integration | newline/tab placement, five-prompt workflow, six-prompt boundary |

### Shared assertion helpers

- `checkEmojiPrompts` (orchestrator) — byte-exact + rune-count + UTF-8-validity + no-U+FFFD for emoji cases.
- `checkNoTruncation` + `firstDiffOffset` (orchestrator) — length-exact comparison that reports the divergence offset instead of dumping multi-kilobyte strings.

## Table-driven pattern

**Confirmed in use.** All 39 functions in `subtask_orchestrator_test.go`,
`subtask_orchestrator_basic_test.go`, and `commands_test.go` follow the standard
Go table-driven shape: an anonymous `[]struct` case slice with a `name` field,
iterated by `for _, tt := range tests { t.Run(tt.name, … ) }`, with expectations
expressed as `wantLen`/`wantPrompts`/`verify` fields (or `want`/`expected`).
The 30 `integration_test.go` functions are single-case direct assertions that
predate the table-driven suite.

## Verification results (2026-08-21)

Run: `go test ./internal/bridge/ -run 'TestSplitParallelPrompts' -v -count=1`

- **69/69 top-level test functions PASS**, 390 table-driven subtests PASS
- **Zero failures, zero panics** (no `FAIL`/`panic` lines in output)
- **Suite time 0.011 s** — parent bead required < 10 s
- Exit code 0

Parent bead `telegram-f65d8ec6` acceptance criteria — all met:

| Criterion | Status |
|---|---|
| All splitParallelPrompts tests pass without errors | ✅ 69/69 |
| Test suite completes in <10s total | ✅ 0.011 s |
| No test failures or panics | ✅ none |
| Coverage: empty inputs and whitespace | ✅ 13 functions (§1) |
| Coverage: delimiter handling | ✅ 24 functions (§2) |
| Coverage: Unicode and emoji preservation | ✅ 13 functions (§3) |
| Coverage: length limits and boundaries | ✅ 11 functions (§4) |
| Coverage: special characters | ✅ 2 functions (§5) |
| Coverage: edge cases | ✅ 7 functions (§6) |

Category counts sum to 70 because `TestSplitParallelPrompts_BasicSplits` covers
both §1 (empty/whitespace cases) and §2 (delimiter cases); 69 distinct functions
in total.
