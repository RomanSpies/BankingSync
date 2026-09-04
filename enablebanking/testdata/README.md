# Enable Banking response fixtures

These files are recorded Enable Banking transactions pages used as contract tests
for the parser. They exist because unit tests written from the parser's own
assumptions are circular: they prove the parser is self-consistent, not that it
understands what Enable Banking actually sends.

## Layout

| File | Meaning |
|---|---|
| `<name>.json` | A captured (and scrubbed) `/accounts/{uid}/transactions` response |
| `<name>.golden.json` | The `Transaction` values the parser is expected to produce |

The fixtures currently committed are **synthetic**, hand-written to cover every
fallback branch in the parser. Replace them with real captures as they become
available — the tests pick up whatever is in this directory automatically.

## Capturing real responses

1. Set the capture directory on the running container and let one sync run:

   ```yaml
   environment:
     EB_DUMP_RESPONSES: "/data/dumps"
   ```

   Each fetched page is written verbatim to `/data/dumps/page-NNNN.json`, and the
   `POST /sessions` reply from the next bank connection to `/data/dumps/session-NNNN.json`.

   The session capture is what tells you where your bank puts the account IBAN.
   `SessionAccount.UnmarshalJSON` probes several container shapes because that
   layout has never been confirmed against a real response; a capture turns the
   guess into a fact. It contains your IBAN and account holder name — scrub it
   with `cmd/ebscrub` before committing anything.

2. **Scrub before doing anything else.** The raw dumps contain real counterparty
   names, IBANs, amounts and narratives — your financial history. This repository
   is public.

   ```sh
   go run ./cmd/ebscrub -in /data/dumps -out enablebanking/testdata/
   ```

   `ebscrub` preserves everything that drives parser behaviour (key names,
   nesting, JSON types, and enums such as `status`, `credit_debit_indicator` and
   `currency`) and replaces identifying values with deterministic synthetic ones.
   Identical inputs map to identical outputs, so dedupe and matching
   relationships survive.

3. **Diff the scrubbed output before committing.** The scrubber is allowlist
   based: unrecognised keys are scrubbed rather than preserved, so it fails
   closed, but a manual read is still the last line of defence.

4. Rename to something descriptive (`ing_booked.json`, `revolut_pending.json`)
   and generate the goldens:

   ```sh
   go test ./enablebanking -run TestFixtures -update
   ```

5. Review the generated `.golden.json`. This is the step that gives the fixtures
   their value — a golden regenerated without reading it only records whatever
   the parser currently does, including its bugs.

## What the tests enforce

- `TestFixtures_parseToGolden` — every record parses to the reviewed values.
- `TestFixtures_noDroppedRecords` — a real payload must never produce a parse
  failure. A drop in production means transactions silently vanish from Actual.
- `TestFixtures_shapeDriftCanary` — a transaction field not listed in
  `knownTransactionFields` fails the build, so an Enable Banking payload change
  is surfaced instead of silently altering behaviour.
- `TestFixtures_parserReadFieldsArePresent` — every field the parser reads is
  exercised by at least one fixture, so no fallback branch goes untested.

When the canary fails, decide whether the parser should read the new field, then
add it to `knownTransactionFields` in `fixtures_test.go`.
