# Wire-format fixtures (protocol format 1)

Test vectors for the names and bodies of `PROTOCOL.md` section 7. The Go reference implementation
and the independent Python client read these same files; neither generates them at test time. This
README is enough to consume them without reading any Go code. The rules the fixtures exercise are
those of `PROTOCOL.md` sections 1, 7.1 to 7.5 and 9; where this file and `PROTOCOL.md` differ,
`PROTOCOL.md` wins and the difference is a bug in the fixtures.

## Layout

```
names.json            names: ticket/hold names and cancel names, accepted and refused
display.json          label -> display suffix examples (informational, see below)
order_keys.json       arrival time and priority -> order key
bodies/ticket/        ticket bodies
bodies/hold/          hold bodies
bodies/cancel/        cancel request bodies
```

Everything is UTF-8 except a few bodies that are deliberately not (named `*invalid-utf8*`).

## Bodies

Each file `bodies/<kind>/<name>.body` is one body, **exactly the bytes a client would find in the
file**. Read it as bytes. Do not parse and rewrite it, and do not normalise newlines or whitespace:
leading and trailing whitespace, key order, escaped spellings of keys (`"cpu"`) and the exact
length are the point of several fixtures. A `.body` file has no trailing newline unless the body
itself has one.

The file name states the expected verdict:

- `ok-<slug>.body`: the body is valid. A decoder must accept it and produce the value in
  `ok-<slug>.expect.json`.
- `bad-<slug>.body`: the body is malformed (`PROTOCOL.md` section 7.2). A decoder must refuse it,
  and a client must treat the entry as ignored. `bad-<slug>.why` holds one line saying which rule
  the body breaks. The reason is for people; no decoder needs to report it.

`ok-<slug>.expect.json` is the decoded value, written as the canonical body of that kind: every key
of the table present, collections empty rather than absent, unknown keys of the source body
dropped, key order and whitespace not significant. Compare it with the value your decoder produced
**as parsed JSON** (object equality), not as text. Resource names in `grant`, `clamped`, `need`
are keys; token indices stay in the order the body gave them (they are ascending).

Verdict reminders, all from `PROTOCOL.md`:

- A body is one JSON object of at most 8192 bytes, optionally surrounded by JSON whitespace
  (space, tab, LF, CR). Anything else, including a byte order mark, a second value after the
  object, or invalid UTF-8, is malformed.
- Every key of the table is required. A missing key, a `null`, a value of another JSON type, or a
  value outside the key's rule is malformed.
- An integer has no fraction and no exponent and fits a signed 64-bit integer: `1.0`, `5e0`
  and `9223372036854775808` are not integers where an integer is required.
- A duplicate key at any depth is malformed, including inside objects and arrays of unknown keys.
  Keys are compared after JSON unescaping.
- Unknown keys, at any depth, are ignored (their values may be any JSON).
- Resource names (the keys of `need`, `grant` and `clamped`, and the elements of
  `unbudgeted_resources`) must match `[a-z][a-z0-9-]{0,31}`.
- `hold` fixtures do not check that `start_ns` equals the start time in the hold's name; that
  comparison needs the name and belongs to the reader of the directory.

## names.json

```
{ "valid":          [ name-case ... ],   ticket and hold names that must be accepted
  "invalid":        [ {"name", "why"} ], ticket and hold names that must be refused
  "cancel_valid":   [ name-case ... ],   cancel request names that must be accepted
  "cancel_invalid": [ {"name", "why"} ]  cancel request names that must be refused }

name-case = { "name": the file name,
              "order_key": 20 decimal digits, as a string (it can exceed 2^53; it is the
                           start-ns of a hold),
              "pid": integer, "seq": integer,
              "display": the display suffix, or null when the name has none }
```

A ticket or hold name is `<20 digits>-<10 digits>-<6 digits>` with an optional `-<display>`
(1 to 32 characters from `A-Z a-z 0-9 . _`). A cancel request's name is the three fields only,
so every cancel name with a display is refused, as listed in `cancel_invalid`. A parser returns
the three numbers; the display is checked against its character set and otherwise ignored. A name
that does not match is not an entry of the directory.

## order_keys.json

A list of `{ "arrival_ns", "priority", "order_key" }`. `order_key` is
`arrival_ns - priority * 60000000000` as a 20-digit zero-padded string.

## display.json (informational)

A list of `{ "label", "display" }`. How a writer derives a display from a label is not part of the
protocol (`PROTOCOL.md` section 7.1) and a reader must never interpret one. These are the
derivations of the Go implementation: every byte outside `A-Z a-z 0-9 . _` becomes `_`, then the
first 32 bytes are kept (a character of two UTF-8 bytes gives two underscores); an empty label has
no display and its name has no `-` suffix. A client may derive its displays differently and remain
conforming.
