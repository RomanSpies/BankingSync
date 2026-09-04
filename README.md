<p align="center">
  <img src="assets/gemini-svg.svg" alt="bankingsync" width="420">
</p>

<h3 align="center">Your European bank transactions, inside Actual Budget or Firefly III. Automatically.</h3>

<p align="center">
  <a href="https://hub.docker.com/r/romanspies/bankingsync">Docker Hub</a> &middot;
  <a href="https://github.com/RomanSpies/BankingSync">GitHub</a> &middot;
  <a href="INSTALLATION.md">Installation Guide</a>
</p>

---

bankingsync connects to your bank via PSD2 open banking and imports transactions into a self-hosted [Actual Budget](https://actualbudget.org) or [Firefly III](https://firefly-iii.org) instance. It runs on your own machine as a single Docker container — no licence keys, no account limits, no phoning home. Your financial data goes from your bank to your server and nowhere else.

## Why bankingsync

- **Broad bank coverage** — works with any bank supported by [Enable Banking](https://enablebanking.com)'s PSD2 integration across Europe
- **Two budgeting backends** — Actual Budget or Firefly III, selected with a single environment variable
- **Connect all your accounts** — add as many bank connections as you need, each mapped to its own budget account (e.g. Revolut → "Revolut", N26 → "N26")
- **No strings attached** — fully open source under AGPL-3.0, no licence keys, no paywalls, no usage caps
- **Self-hosted end to end** — bankingsync runs on your machine and talks only to Enable Banking and your budget backend. The project operates no servers, no hosted UI and no account: see [Where your data goes](#where-your-data-goes)
- **Read-only access** — bankingsync uses PSD2 read-only consent, it cannot initiate payments or modify your bank account in any way
- **Opening balances** — the money that was on the account before the first import is carried over, so the budget balance matches the bank. Written once, then only checked: a later difference is reported, never silently corrected
- **Pending-to-cleared lifecycle** — pending transactions are imported immediately and automatically promoted to cleared once they settle
- **Built-in deduplication** — transaction references are persisted per bank account, and a proximity match catches transactions whose reference, amount or payee changes between the pending and booked stage (hotel and fuel pre-authorisations, card scheme prefixes)
- **Rules run automatically** — categorisation and payee rules are applied to newly imported transactions. With Actual, bankingsync evaluates them itself and supports category, payee, notes and cleared. With Firefly III, its own rule engine runs server-side on booked transactions
- **Email notifications** — get alerted on sync failures and before a bank session needs to be re-authorised, with a test email button to verify your setup
- **TLS out of the box** — a self-signed certificate is generated on first start so the web UI is always served over HTTPS
- **Full observability** — ship OpenTelemetry metrics and traces to your collector, and continuous profiling data to Grafana Pyroscope
- **Supply chain transparency** — every container image ships with a CycloneDX SBOM (Go modules + OS packages) viewable in the web UI, downloadable as JSON, and attached as a BuildKit attestation on Docker Hub
- **Minimal footprint** — single Go binary, single Docker container, SQLite for storage, zero runtime dependencies

## How it works

```
Your bank
   |  (PSD2 account information access)
   v
Enable Banking            <- a third party; see "Where your data goes"
   |  (read-only REST API)
   v
bankingsync (on your machine)
   |
   |--- Fetches transactions since last sync
   |--- Filters out already-imported transaction IDs
   |--- Writes new transactions to your budget backend
   |--- Promotes pending transactions to cleared when they settle
   |--- Applies your budgeting rules to new transactions
   |--- Sends an alert email if anything goes wrong or a session is expiring
   |--- Logs the result to the sync history
   v
Your Actual Budget or Firefly III instance
```

On first run, bankingsync imports the last 30 days of transactions. After that, it syncs only new data on every cycle. The sync interval is configurable (default: every 6 hours).

## Where your data goes

**Enable Banking is a third party, and your transaction data passes through it.**
That is not a bankingsync design choice you can opt out of: under PSD2, banks
expose account information to licensed account information service providers,
and Enable Banking is the AISP this project uses. Your bank sends your
transactions to them, and bankingsync reads them from their API.

What bankingsync adds to that path is nothing:

| Party | Sees your transactions | Operated by |
|---|---|---|
| Your bank | yes | your bank |
| Enable Banking | yes | Enable Banking Oy (Finland) |
| bankingsync | yes | **you**, on your own hardware |
| Your budget backend | yes | **you**, self-hosted |
| The bankingsync project | **no** | — |

There is no hosted component, no account to register with this project, no
telemetry sent anywhere by default, and no relay through infrastructure the
maintainers control. The OpenTelemetry and Pyroscope exporters are opt-in and
point at endpoints you choose.

**Decide for yourself whether you trust Enable Banking.** They hold a licence
and their [terms and privacy policy](https://enablebanking.com/legal/) govern
what they may do with the data your bank hands them. That is a judgement you
should make deliberately rather than inherit from this README. If you are not
comfortable with it, no self-hosted PSD2 client can fix that — the intermediary
is structural, not incidental.

## Quick start

> For a detailed walkthrough, see [INSTALLATION.md](INSTALLATION.md).

### 1. Set up Enable Banking

[Enable Banking](https://enablebanking.com) is the regulated open banking provider that connects bankingsync to your bank.

1. Sign up at [enablebanking.com](https://enablebanking.com)
2. Register a new application in the developer portal
3. Generate your RSA key pair — either let Enable Banking generate it for you during app registration (a `.pem` file will be saved to your Downloads folder), or create one yourself:

```bash
openssl genrsa -out private.pem 2048
openssl rsa -in private.pem -pubout -out public.pem
```

   If generating manually, upload `public.pem` to the developer portal.

4. Add `https://localhost:8443/callback` as an allowed redirect URI

### 2. Point bankingsync at your budget

**Actual Budget** — go to **Settings > Sync > Show file ID** and copy the value into `ACTUAL_SYNC_ID`.

**Firefly III** — go to **Options > Profile > OAuth > Personal Access Tokens**, create a token and copy it into `FIREFLY_TOKEN`. Set `BUDGET_BACKEND: "firefly"` and `FIREFLY_URL` to your instance.

### 3. Start bankingsync

Create a `docker-compose.yml`. bankingsync waits for Actual Budget to be healthy before starting its first sync:

```yaml
services:
  actual:
    image: actualbudget/actual-server:latest
    restart: unless-stopped
    expose:
      - "5006"
    volumes:
      - actual_data:/data
    healthcheck:
      test: ["CMD-SHELL", "node src/scripts/health-check.js"]
      interval: 30s
      timeout: 5s
      retries: 3

  bankingsync:
    image: romanspies/bankingsync:latest
    restart: unless-stopped
    depends_on:
      actual:
        condition: service_healthy
    expose:
      - "8443"
    volumes:
      - bankingsync_data:/data
    environment:
      ACTUAL_URL: "http://actual:5006"
      ACTUAL_PASSWORD: "your-password"
      ACTUAL_SYNC_ID: "your-sync-id"

volumes:
  actual_data:
  bankingsync_data:
```

For Firefly III, replace the three `ACTUAL_*` variables with:

```yaml
      BUDGET_BACKEND: "firefly"
      FIREFLY_URL: "http://firefly:8080"
      FIREFLY_TOKEN: "your-personal-access-token"
```

See the included [`docker-compose.yml`](docker-compose.yml) for a full example with all optional parameters (email notifications, sync interval, observability, networking, etc.).

```bash
docker compose up -d
```

### 4. Complete setup in the browser

Open **https://localhost:8443** (accept the self-signed cert warning).

If bankingsync is on a remote machine:

```bash
ssh -L 8443:[DOCKER_CONTAINER_IP]:8443 yourserver
```

The web UI walks you through four steps:

1. **Setup** — upload your `private.pem` and enter your Enable Banking Application ID
2. **Connect** — pick your country and bank, complete the OAuth flow
3. **Pick Account** — choose which bank sub-account to sync (showing IBAN, owner, and currency when available), which Actual Budget account to import into, and from which date to start importing
4. **Status** — see your connected accounts, sync history, and watch the first sync run

That's it. bankingsync syncs automatically from here on. Connect additional banks any time from the Connect page — each one maps to a different Actual Budget account.

## Security

**The web UI has no authentication.** Every endpoint — including the Enable
Banking private key upload, account removal and manual sync — is reachable by
anyone who can open the port. Treat the listening socket as the only access
control there is.

- Bind to loopback (`127.0.0.1:8443:8443`) and reach it over an SSH tunnel, or
  put it behind a reverse proxy that provides authentication. The bundled
  `docker-compose.yml` uses `expose:` rather than `ports:` for this reason.
- Never expose the port to a LAN you do not fully trust, and never to the
  internet.
- Cross-origin `POST` requests are rejected, so a malicious page you visit
  cannot drive the UI on your behalf. This is a defence against drive-by
  requests, not a substitute for authentication.
- `X-Forwarded-Proto` and `X-Forwarded-Host` are ignored unless
  `TRUSTED_PROXY=true` is set. Only enable it when a reverse proxy you control
  sets those headers, otherwise a client can choose the URL that appears in
  session-expiry emails.

### Secrets at rest

The Enable Banking private key is stored unencrypted in `bankingsync.db`
inside the `/data` volume, and `ACTUAL_PASSWORD` / `SMTP_PASS` are passed as
environment variables (visible via `docker inspect`). Protect the volume and
the compose file accordingly.

### Actual Budget compatibility

bankingsync drives Actual Budget's **internal sync protocol and SQLite schema
directly**. Neither is a documented, stable API, so an Actual upgrade can break
synchronisation. Pin the Actual version you have tested against, and check the
sync history after upgrading either side.

**End-to-end-encrypted budget files are not supported.** bankingsync fails at
startup with a clear error if the budget is encrypted.

### Firefly III compatibility

bankingsync uses Firefly III's documented v1 REST API, which is a far safer
footing than the Actual integration. One dependency is not covered by that
guarantee: deduplication looks a transaction up with the **search operator**
`external_id_is`, which belongs to Firefly's search DSL rather than its API
contract. If that operator is ever renamed or degraded, the lookup returns
nothing and the transaction is imported a second time — silently. bankingsync
verifies the `external_id` it gets back rather than trusting the operator, which
turns a silent duplicate into no match at all, but the risk is worth knowing.

Account lookup rests on the same kind of footing and is handled the same way:
`GET /v1/search/accounts` matches with `LIKE '%query%'` for both `iban` and
`name`, so a hit proves nothing on its own. bankingsync re-checks that the
returned IBAN or name is exactly the one it asked for, which is what stops
`Checking` from adopting `Old Checking`.

Both of those are checked rather than assumed. Every merge request runs the full
Firefly suite against a real Firefly III container — the current `latest` image,
not the test fixture — and the pipeline fails if it does not pass. That is how the
two behaviours above are known to hold today rather than on the day they were
written.

Tested against Firefly III **v6.6.6**. Check the sync history after upgrading.

### Firefly III — differences from Actual Budget

| | Actual Budget | Firefly III |
|---|---|---|
| **Rules** | bankingsync evaluates them itself, immediately, on exactly the transactions it touched | Firefly's own engine runs server-side, only on **booked** transactions. Pending ones are deliberately excluded, because a rule could strip the pending tag before the booking ever confirms it |
| **Pending** | the `cleared` flag | a tag (`pending` by default). The tag is a display aid, not the source of truth — deleting it does not stop the transaction from being confirmed |
| **Manual entries** | a manually entered transaction is adopted by the bank import | adopted too, **unless** it already carries an `external_id` from another importer. Then a second transaction is created rather than overwriting someone else's record |
| **SEPA references** | not stored | `EREF`, `MREF` and `CRED` are written to `sepa_ct_id`, `sepa_db` and `sepa_ci` |
| **Own transfers** | imported as a payment to a payee | when the counterparty IBAN belongs to another of your asset accounts, imported as a real `transfer` |
| **Account matching** | by name | by IBAN first, name only as a fallback. Firefly keeps asset IBANs unique per user, so **renaming an account in Firefly is safe** — the next sync finds it again by IBAN. Under Actual the name is the only handle, so a rename has to be mirrored in the web UI |
| **Split transactions** | untouched | if you split one of our transactions, bankingsync stops updating it rather than deleting your splits |
| **Reconciled** | never modified | never modified |
| **Opening balance** | one cleared transaction, payee "Starting Balance", **uncategorised** — on a budgeted account it shows up in "To Budget" | a native account property (`opening_balance`), no transaction and no budget effect |

### Opening balances and drift

bankingsync imports transactions from a window, so on its own the budget would
only ever show the money that moved inside that window. To make the balance
match the bank, an **opening balance** is written once per account:

```
opening balance = the bank's booked balance − the booked transactions we imported
```

A **booked** balance is preferred: `CLBD`, then `ITBD`, then `PRCD` when it
carries a reference date. Pending transactions are not subtracted from one,
because it does not contain them — the budget therefore leads the bank's booked
balance by exactly your outstanding authorisations, which is correct: the money
is gone, it is just not posted yet.

Some banks report **no booked type at all** — Revolut answers with `ITAV` only.
Those accounts fall back to an available balance (`ITAV`, then `CLAV`), and the
arithmetic changes with it: an available balance already reflects card holds, so
pending transactions are subtracted as well. The balance type used is shown
before anything is written and recorded afterwards.

> **One caveat you should check.** At a bank that grants an overdraft, an
> available balance also includes the credit line, and the line is not in the
> API response. If your account has a Dispo and your bank reports only available
> types, the opening balance will be too high by that amount. Compare the figure
> against your banking app before confirming, and correct it in your budget if
> it is off. Accounts with a booked balance are unaffected.

Balances that are wrong in kind stay refused: `OPBD`/`OPAV` are as of the start
of a period, `FWAV`/`XPCD` include entries that have not happened, and
`VALU`/`INFO`/`OTHR` have no fixed meaning. If nothing usable is reported, the
account is marked `n/a` and nothing is written.

**It is written once and never adjusted.** Every later sync compares the two and
reports the difference:

| State | Meaning |
|---|---|
| `ok` | budget and bank agree |
| `drift` | they disagree — shown on the dashboard, in `/health` and as a metric |
| `unstable` | the balance moved during the run, so no comparison was possible |
| `incomplete` | a transaction was dropped or a write failed this run |
| `no_opening_balance` | nothing to compare against yet |

Drift is never corrected automatically. A deleted transaction, a manual edit or
a transaction outside the retention window are all legitimate reasons for it, and
guessing which one applies would do more harm than reporting it. An email is sent
when the difference exceeds the threshold in **two consecutive syncs** — a
settling authorisation looks like drift for exactly one cycle.

**Existing installations are not touched.** Accounts connected before this
feature existed keep whatever balance they have; use **Set** in the account table
to add an opening balance after reviewing the figure. New connections get one on
their first clean sync.

**Balance access is granted at authorisation time.** If an account shows
`no access`, use **Renew** to re-authorise that bank — Enable Banking cannot widen
the consent afterwards. Set `EB_REQUEST_BALANCE_ACCESS=false` if your bank refuses
an authorisation request that asks for balances.

### How a transaction is recognised

Before any matching happens, an incoming transaction needs an identity that
survives to the next run — the bank offers the same rows for days, and something
has to say "this one again" rather than importing it twice.

A bank reference is used when there is one. When there is not — the ordinary case
for pending rows at many institutions — the identity is built from the date, the
amount, the payee and an occurrence index. All four are needed:

- **the payee**, because without it two different shops charging the same amount
  on the same day share an identity and the second is dropped as a repeat of the
  first
- **the occurrence index**, because two identical purchases on the same day are
  two transactions, not one

The index makes identity positional, and position is only stable while the set
is. The payee is what makes it a property of the transaction instead: let one
purchase drop out of the window and a new one appear, and without the payee every
index after the gap shifts by one and the newcomer inherits an identity that is
already imported. A statement whose rows are returned in a stable order per day
is the one remaining assumption, and a broken one produces a visible duplicate
rather than a silent loss.

Rows are processed in a fixed order — date, then status, then amount, payee and
reference — so a run over the same statement is a run over the same order.

### Pre-authorisations and duplicates

A card payment often arrives twice in different clothes: authorised as
`Hotel Berlin` for 120.00, then booked as `VISA Hotel Berlin` for 138.50, with a
different reference. Matched strictly, that is two transactions in your budget.

Deciding whether two rows are the same payment is **record linkage**, and it has
a standard model: Fellegi–Sunter (1969), the same one behind Splink and national
statistics offices. bankingsync implements it rather than inventing a similarity
score, because the parameters of the published model are things you can argue
with and a number pulled from the air is not.

Each candidate row in the fifteen-day window is compared field by field. Every
field yields a **comparison level** — a category, not a position on a scale:

| Field | Levels |
|---|---|
| Payee | `exact`, `truncated`, `fuzzy`, `subset`, `conflict`, `none`, `missing` |
| Amount | `exact`, `higher_within`, `lower_within`, `outside_higher`, `outside_lower` |
| Date | `same`, `after_near`, `before_near`, `after_far`, `before_far` |

Each level carries a weight, `log₂(m/u)`: how often that level occurs when two
rows really are the same payment, against how often it occurs by chance. The
weights add up, and the total converts to a probability. That is the whole
mechanism, and it is why a strong payee agreement can make up for a weaker amount
agreement instead of every field having to pass its own gate.

**Why levels and not a score.** `Shell Tankstelle` / `VISA Shell` is one name
with a word cut off — the same shop. `Da Luigi Roma` / `Visa Da Luigi Milano`
contradicts a word — a different branch. Under Sørensen–Dice both score 0.667,
and no weighting of Dice against Jaro–Winkler separates them either. Truncation
*removes* a word; a different branch *contradicts* one. That is a difference in
shape, not in degree, so it belongs in the levels.

`missing` is a level of its own. An absent payee is not disagreement, and it
weighs neither for nor against.

**The direction of the amount and the date is recorded but carries no weight of
its own.** A booking above its authorisation is a tip or a hotel incidental; one
below it is a fuel release. A booking dated before the authorisation it settles
is either impossible or routine depending on which date the institution reports.
How often each happens is not known here, so the shipped parameters give the two
halves of each level identical weight and the distinction is only collected. It
is collected so that it can stop being a guess: an installation whose decision log
has counted enough of them can fit the halves apart and put the result into force
through the gate below. Nothing ships with them separated, because separating them
from no evidence is the one thing this model exists to avoid.

**An exact agreement on a common payee says less** than one on a rare payee. The
account's own payee distribution is measured from the statement each run and
applied as a correction to the payee weight, floored so that a name seen once
cannot outweigh the rest of the model. Below fifty transactions the correction
stands down: a frequency drawn from a handful of rows is a coincidence, not a
distribution.

Three hard rules run **before** the model and are not probabilistic:

- a bank reference that already matches is a lookup, not a guess
- a settled row carrying somebody else's reference is never re-adopted
- a row already settled by a bank reference in this batch is not on offer

### The batch is decided together

Transactions are not weighed one at a time. All the ones that need the model are
collected, and the arrangement of the whole batch is chosen under a **one-to-one
constraint**: a budget row can settle at most one incoming transaction. This is
Jaro's (1989) formulation of the linkage problem as an assignment, solved with
the Hungarian method.

It fixes two things a pairwise reading cannot:

- **Order no longer decides.** Two bookings that both fit one authorisation used
  to be settled by whichever the loop reached first, so the same statement served
  in a different order produced a different budget.
- **Two identical settlements both merge.** Two purchases of the same amount at
  the same shop on the same day, each authorised and each settling, produce one
  arrangement that uses both authorisations. Choosing which booking takes which
  is not a decision — the budget is identical either way.

Unequal counts stay a question. Two authorisations with one settlement between
them means one is still outstanding, and which is not decidable from anything in
the statement.

**Ambiguity is now measured against the arrangement**, not the pair: how much
total evidence the batch loses if a pairing is forbidden. A candidate that looks
like a close second is not a real alternative when another transaction has a
stronger claim on it, and asking about that is asking about a choice that was
never open.

**The prior counts plausible candidates, not rows.** With *n* candidates at most
one is the settlement, so the starting point is 1/(*n*+1) — the "+1" being "or
none of them", since most incoming transactions are simply new. But *n* is how
many candidates the evidence does not already rule out, not how many rows happen
to share the fortnight. Counting the whole window punished the winner for the
company it kept: measured, a payee agreeing exactly and an amount agreeing to the
cent stopped being merged automatically once fifteen unrelated rows shared the
window, because the prior cost 3.9 bits. Which candidate wins among several is
the assignment's question and the margin answers it.

### The review queue

The model produces a probability, and there are two thresholds rather than one:

- **at or above the auto threshold** — merged without asking
- **below the review threshold** — treated as a new transaction and imported
- **between them** — held back and listed under **Review**

The middle band is the "clerical review" zone the Fellegi–Sunter model has had
since 1969. A held transaction is **not written to the budget at all** until you
decide, because both available guesses are worse than asking: importing it leaves
a duplicate somebody has to find, and adopting it may overwrite an unrelated
authorisation, which nobody would find.

Two answers are offered per row: *it belongs to this candidate* (merged, keeping
the booked amount and reference) or *it is new* (imported on its own). Each
candidate is shown with its probability **and the levels behind it** — "amount to
the cent · payee cut short · 2 days apart" — because a number you cannot argue
with is not a decision aid.

Candidates are recomputed each time the page is drawn, never stored: a row saved
days ago may since have been split, edited or deleted. The probability you saw is
sent back with your answer and checked against a fresh one, so a decision made
against a stale page is refused rather than applied.

The page also carries the identity of the parameter set that produced it. A
threshold does not enter any probability, so moving one changes what the page
means without changing a figure on it — and the "this is new" answer never had a
figure to check in the first place. Both are refused when the settings have moved
since the page was drawn.

Because a held transaction is money in no budget, it is visible in five places:
the dashboard, `/review`, `/health` (`degraded` while anything is open), the
`bankingsync_match_reviews_open` gauge, and one email per run that held
something. It also **defers the opening balance** for that account — that figure
is written once and never revised, so a held amount absorbed into it would stay
absorbed — and suppresses the drift alarm, which would otherwise measure the
queue rather than the account.

Removing a bank account clears its held transactions along with its other
per-account state; resetting the import state clears them all.

Everything is tunable under **Settings**:

| Setting | Default | Meaning |
|---|---|---|
| Amount tolerance (percent) | `25` | of the larger of the two amounts; `0` requires an exact match |
| Amount tolerance cap | `5000` (50.00) | absolute ceiling, whichever is smaller |
| Card scheme prefixes | `VISA,MASTERCARD,MC,MAESTRO,DEBIT,KARTENZAHLUNG,POS` | ignored when comparing payees |
| Merge automatically from | `90`% | at or above this, rows are merged without asking |
| Ask from | `50`% | below this, the transaction is imported as new; between the two it is held for review |
| Ask about one automatic decision per sync | off | see [Asking about a decision made without you](#asking-about-a-decision-made-without-you) |
| Drift alert threshold | `1000` (10.00) | `0` disables the email |

The review threshold may not exceed the automatic one — that would leave no band
to ask in, and everything would be merged silently. Setting them equal is legal
and is the narrowest useful setting: only genuine ties are then asked about.

The amount tolerance default covers tips and hotel pre-authorisations. A fuel
release of 100.00 that books at 60.00 differs by 40% and is **not** within
tolerance — but under the model that is heavy evidence against rather than an
automatic refusal, so a matching payee on the same day can still carry it into
the review band. Payees are compared with card scheme prefixes removed; what
gets written is still what the bank sent.

When a transaction is created although something in the window nearly matched,
the reason is logged and counted in `bankingsync_near_miss_total`: `amount` for a
row close in value, `payee` for one the payee levels refused, `date` otherwise.

**`ambiguous` no longer appears there under the default configuration.** Two rows
that fit equally well used to produce a duplicate and a counter tick; they are
now held for review instead, which is better — but it means that case shows up as
`bankingsync_match_reviews_total{reason="ambiguous"}`, not as a near miss. The
review counter carries `reason` for exactly this: `ambiguous` means forbidding
the chosen pairing would cost the batch almost nothing, so some other arrangement
is as good; `uncertain` means the pairing stands on its own but is not convincing
enough, and moving a threshold would change it.

`ambiguous` has meant two things in two releases. It was the gap between the best
and second-best *candidate*; since the batch is arranged as a whole it is the gap
between the best and second-best *arrangement*, which is the same question asked
where it can be answered. A dashboard reading that series across the upgrade is
comparing two measurements.

To see where your own bank's transactions fall before moving a threshold, use
`bankingsync_match_probability` — one observation per incoming transaction,
labelled by what was decided. If nothing sits between your two thresholds the
review band is doing nothing; if a large share does, it is set too wide for that
bank. Transactions with no candidate at all are not recorded, so the histogram is
the distribution of real comparisons rather than a spike at zero.

### What the matching writes down

Every decision is recorded — not only the ones put to a person. The band you see
is the smallest part of the traffic and the least interesting: the automatic band
is where the expensive mistakes happen and where nobody is looking, and a model
later estimated on the doubtful cases alone would be biased by exactly the cases
it never saw.

The record holds the comparison levels, the weight, the probability, the margin,
the number of candidates and the identity of the parameter set — **no payees and
no amounts**. When a match goes wrong, the levels are the answer to the first
question without anybody having to send account data around. When a person
resolves a held transaction, their answer is written back to the record: it is
the only observation available that does not come from the model itself.

Records are kept for the same window as everything else and dropped with the
account they belong to.

### Is the number honest?

Ranking and calibration are different properties. A model can order true matches
above false ones perfectly and still be systematically overconfident, and no
ranking metric will show it — a threshold of ninety per cent on a model really
running at seventy is a threshold nobody chose.

Once enough decisions have been settled by something other than the model — see
the three sources below — two figures are reported:

- `bankingsync_match_brier_score`, split the way Murphy (1973) split it into
  **reliability**, **resolution** and **uncertainty**, plus the two within-bin
  terms Stephenson, Coelho and Jolliffe (2008) showed the three-way split needs
  once you bin. The split is what makes the number useful: rescaling the
  probabilities lowers reliability, and only a model that separates matches from
  non-matches better raises resolution. Without it, moving the scale and
  improving the model look identical. Plot `reliability − generalised_resolution
  + uncertainty` if you want it to close against `score` — the three-component
  form does not, by an amount that depends only on the bin count.
- `bankingsync_match_calibration_error`, one number for an alarm.

Below fifty settled decisions neither is reported. A Brier score over a dozen
answers is noise with a decimal point, and a missing series is honest where a
wrong one gets alerted on. The calibration error needs three hundred rather than
fifty, because the binned estimator is biased upwards and the bias does not
vanish on a correct model: at a hundred and twenty settled decisions a perfectly
calibrated matcher reports about 0.064. An alarm that fires on a correct model is
worse than no alarm.

The correction, when there is data to fit one, is **Platt scaling** — a logistic
regression with the raw match weight as its only feature, `P = 2^(A·M+B)`. Two
parameters need one or two orders of magnitude less data than the thirty-four
level probabilities do, and the division of labour stays honest: `m` and `u`
remain the arguable claims about how banks behave, and `A` and `B` absorb what is
wrong with the model — chiefly its assumption that the fields are independent,
which they are not. It ships as the identity, `A=1, B=0`, which is exactly the
arithmetic that was there before it existed, and a fitted one only ever replaces
it through the gate below.

### Where the answers come from

A model that only ever learns from the cases it was unsure about is biased by the
cases it was not — the same blind spot credit scoring calls reject inference. Two
sources are recorded, and neither is the model's own opinion:

- **A person's answer** to a review. Covers the band, and only the band.
- **A pair the bank's own key settled.** When a pending entry is confirmed by its
  reference, the bank has stated that two rows are one payment. What the model
  *would* have made of that pair is worked out and written down beside it, which
  measures exactly the merges the matcher would have missed. That figure —
  `bankingsync_match_labels_total{source="reference_model_agreed"}` — is worth
  having whether or not anything is ever fitted to it.

  **This only counts as evidence when your bank supplies the reference.** Without
  one, the key a pending entry is matched on is built from the date, the amount
  and the payee — every field the model compares. A pair selected that way agrees
  on those fields because the key said so, so fitting to it would ask the model to
  confirm the rule that picked the pair. Those confirmations are still recorded
  and still counted, under
  `bankingsync_match_labels_total{source="fallback_key"}`, because a merge the key
  caught and the matcher would have missed is worth knowing about either way — but
  their outcome is left unanswered so that nothing ever fits to them. On a feed
  with no references this source contributes no evidence at all, and the other two
  are the only ones you have.

- **A confirmation you were asked for.** Off by default. When it is on, each sync
  picks one decision it made on its own and asks about it under Review. This is
  the only source that reaches the automatic bands, which is where the expensive
  mistakes are and where nothing would otherwise ever contradict the matcher.

**Balance drift is deliberately not a third source.** It looks like one: a wrong
merge makes an amount vanish, a wrong duplicate leaves one too many, and the
difference is directional. But drift is suppressed for any account with an open
review — by design, since the budget is then short on purpose — so there is never
a run in which a decision is open *and* a drift figure exists. There is also no
drift history to difference. Both would have to change first, and each is its own
decision with its own risks.

### Asking about a decision made without you

Almost every transaction is settled without anybody being consulted, and nobody
ever finds out whether those decisions were right. A wrong merge above the
automatic threshold takes two payments and leaves one, and nothing in the program
will ever say so. The review queue cannot help: it only ever produces answers
about the band it asks about.

With **Ask about one automatic decision per sync** switched on in Settings, a run
picks a single such decision and puts it on the Review page as a question. It is
not a queue item. The transaction is already in your budget, nothing waits on the
answer, and the answer changes nothing about it — a "no" teaches the matcher and
leaves the budget alone, so correcting the budget itself, if it needs it, is a
manual edit. There is always an **I cannot say**, and it is the right button
whenever you cannot remember: a guessed answer is worse than none, because it is
about to be weighed against a stated prior that at least had an argument behind
it.

**Which decision gets asked about** is not the least confident one — that is the
review band, and those are answered anyway. It is the one whose answer would say
the most about the parameters:

    I(answer; parameters) ≈ (ln2 / 2) · Var(M) · P(1 − P)

Var(M) is how badly the levels this decision rested on are known, which under the
Dirichlet posterior above falls as observations arrive and starts out largest for
the levels the shipped priors spread thinnest. P(1 − P) is how much the answer was
in doubt: a decision at 99.9% has a foregone answer and teaches nothing whatever
its parameters look like. Neither factor alone picks the right question, and the
product is not a weighting anybody chose — it is what expanding the mutual
information between the answer and the parameters to second order gives.

What that selects is the frontier just outside the review band: decisions
confident enough to have been acted on alone, resting on level combinations this
installation has little evidence for. One per sync, and never a second while one
is unanswered — a nightly sync would otherwise stack up a month of questions, and
a queue of questions is something people learn to click past.

### Handing over a diagnosis

`bankingsync export-linkage-stats` writes a file describing how an institution
behaves: how often each comparison level occurred, how often it turned out to be
a match, the observed payee field width, and the outcomes. **No payees, no
amounts, no dates finer than a month, no bank names, no account identifiers, no
identifier for the installation.** It goes to standard output; there is no server
and no network traffic, and what happens to the file is the operator's decision.

It exists because the shipped parameters can otherwise only ever describe banks
the author has. An institution that truncates its payee field produces evidence
nobody else has, and this is how that evidence can reach an issue.

### Learning from them

When there are enough settled decisions, the parameters can be re-estimated
rather than left as stated priors:

    m̂ᵢ = (α·priorᵢ + cᵢ) / (α + Σcⱼ)

The Dirichlet posterior mean, with the shipped priors as the base measure and
α = 1 — add-one smoothing, the standard weak choice for a multinomial and the
only one here with a source behind it. With no observations it is the prior
**exactly**, so an installation that never labels anything gets today's behaviour
and not an approximation of it. The distribution still sums to one by
construction, so the constraint that keeps these parameters from being free
numbers survives the fitting.

α was fifty, meaning "it takes fifty observations to overrule a stated claim by
half". Nothing about how banks behave justifies fifty, and measured against known
truth by simulation it was worse than not refitting at all on a small corpus.

A level that never appears among the observations comes back **rarer** than
claimed rather than held where it was, and that is the point rather than a
defect: drawing a hundred observations and seeing none of a level is evidence
that the level is rare. It never reaches zero, because silence is not
impossibility and a zero would send that level's weight to infinity. The two
sides shrink by different amounts, since `m` is fed by labels numbering in the
tens and `u` by sampling numbering in the thousands — which is the honest
statement that far less is known about one side than the other, not an artefact
to be corrected away.

**The two halves of the model come from different places, and they have to.**
The `m` probabilities describe pairs that are one payment, and only something
outside the model can establish that — a person's answer, or a bank's own
reference. The `u` probabilities describe pairs that are not, and those need no
answer from anybody. Almost every candidate a transaction is weighed against is
a non-match: at most one row in a window can be its counterpart, so every other
one is a draw from exactly the population `u` describes. Where a row was actually
merged into, it is left out, and what remains is contaminated only on the order of
the matcher's error rate rather than of the base rate. Where nothing was merged —
a window put up for review, or one that produced a new transaction — every
candidate is counted, because there the model has made no claim about which row
was which and leaving the best-looking one out would presume the answer.

Estimating `u` without labels is standard practice in record linkage, and it is
what makes a refit reachable here at all. **The estimator is not the same one
Splink uses**, and the difference is worth stating: Splink samples records at
random and compares the full cartesian product of them, with no blocking at all.
This samples candidates inside a fifteen-day window on one account. That is
internally consistent — the `u` probabilities in this model are *defined* as
within-window rates and are only ever applied to within-window pairs — but it is a
different quantity from Splink's, and a number from one is not a number from the
other. The label sources in service produce
almost nothing but confirmed matches — a bank reference only ever says "these two
are one payment" — so left to those the `u` side would sit on its stated priors
however long an installation ran, and a refit could only ever adjust half the
model.

The sample is counted, never stored row by row: a level name and a number, with
no payee, amount or date attached. It is discarded when the amount tolerance, the
payee prefixes or the date window change, because those are what decide which
level a pair reaches, and a tally gathered under other rules describes a
classification that is no longer being made. Nothing else touches it — promoting
a fitted parameter set changes what a level is *worth*, not which level a pair
falls into, so a promotion keeps the sample that made it assessable. Transactions the matcher would not decide contribute
nothing — where it did not settle which candidate was the match, it cannot call
the others non-matches either.

The estimate is installation-wide rather than per bank. A promoted parameter set
applies to every connected account.

Nothing fitted this way ever takes effect on its own. It has to go through the
gate below, and a person has to press the button.

### Changing the parameters is a decision, not a result

The **Matching** page shows the parameter set in force, the one the evidence
supports, and what is known about the difference. Nothing there happens
automatically, and the reason is that a parameter change is a change to how money
is matched: a set that scores better on average can still be worse on the case
that actually matters to somebody.

**The prior a refit starts from is always the shipped set**, never what happens
to be in force. Otherwise each promotion would fold the same evidence in a second
time and the levels would walk further from their priors round after round with
nothing new having been observed.

**Watching comes first.** A candidate is evaluated beside the real decision on
every sync: the same batch is arranged a second time under the candidate
parameters, and the difference is written down. Nothing is acted on, and it costs
no extra reads of the budget — the candidate rows have already been fetched.
Until a candidate has been watched it cannot be promoted, because there would be
no record of what it would have changed.

**Three findings, and they are different in kind.**

- **Anchor cases** — six documented behaviours, listed below, run through the
  real decision function under the candidate parameters. This one is absolute. A
  candidate that stops recognising a truncating bank's own settlements has
  changed what the program is for, and no score buys that back. It is a separate
  check rather than a term in one for exactly that reason.
- **Calibration** — the Brier score of the candidate against the set in force, on
  held-out evidence. The candidate is refit on part of the settled decisions and
  judged on the part it never saw; fitting and scoring on the same data flatters
  any procedure whatever, including a bad one. Below fifty settled decisions this
  is reported as *undecidable*, which blocks promotion — "we cannot tell whether
  this is better" is not a reason to install something.
- **Changed decisions** — how many decisions would have gone differently. This one
  is a fact, and whether that many is acceptable is a judgement. The program
  states the number and declines to grade it.

The way back exists on the same page. A change to how money is matched that
cannot be undone where it was made is one nobody should be encouraged to make.

#### The six anchors

These are promises rather than test cases. Any parameter set this program runs,
shipped or fitted, decides all six the same way:

| Case | Outcome |
|---|---|
| An authorisation and its booking agreeing on name, amount and day | merged |
| A truncated payee settling its own authorisation to the cent | merged |
| An authorisation carrying no payee at all, exact amount, same day | merged |
| The payee replaced by an unrelated name, exact amount, same day | asked about |
| The same payee, an amount that drifted, a week apart | asked about |
| The same payee and nothing else in common | imported as new |

Each sits well clear of a threshold. A case balanced on one belongs in the
sensitivity table, which exists to count them; an anchor that failed on rounding
would say nothing about whether the parameters are still sound.

### How much the parameters matter

`budget/sensitivity.md` in the source tree is generated, not written. It varies
every parameter by a factor of three in both directions — rescaling the rest of
its distribution so it still sums to one — and counts how many decisions over a
synthetic statement change as a result.

It exists because calibration cannot be measured before release: that would need
ground truth from an installation running the thing. What can be measured is
whether the answer would move if the numbers were wrong. Where it would not,
their being stated priors rather than estimates costs nothing; where it would,
the table names the level, and the honest response is a wider review band at that
point rather than a better guess.

A change to that table fails the build until somebody has read it.

### Switching backends

The deduplication state in `imported_refs` and `pending_map` belongs to whichever
backend wrote it. Changing `BUDGET_BACKEND` on an existing installation therefore
**refuses to start**: continuing would silently skip every transaction already
imported into the old backend, so none of them would ever reach the new one.

To go through with it, start once with:

```yaml
      BUDGET_BACKEND: "firefly"
      BUDGET_BACKEND_MIGRATE: "true"
```

bankingsync then discards that state along with the sync watermarks, logs how
many entries it dropped, and re-fetches from the bank into the new backend.
Remove `BUDGET_BACKEND_MIGRATE` again after the first successful sync.

**Be clear about what a migration can and cannot do.** bankingsync stores no
transactions of its own — only the deduplication state that says which bank
references have already been imported. It cannot replay your Actual history into
Firefly, because it never had it. All it can do is ask the bank again, so the new
backend gets whatever Enable Banking still serves:

- **By default, the last 30 days.** That is the fallback window, and it is what
  you get unless you say otherwise.
- **Set `start_sync_date` per bank in the web UI before the migration run** to
  reach further back. How far you actually get is your bank's decision — under
  PSD2, 90 days of history is typical, and some banks serve less.

Everything older than that stays only in the old budget. **Transactions already
in the old budget are not removed** — if you keep both, the overlapping window
will exist in each.

Switching back to Actual is the safer direction: Actual identifies transactions
by a stored `financial_id` across its entire history, so duplicates are caught
even without the deduplication state. Firefly relies on the `external_id_is`
search described above, which makes a rollback *to* Firefly the riskier one.

## Configuration

Deployment is configured by environment variables, and only three are required.
**Matching is not among them.** The thresholds, the amount tolerance, the payee
prefixes and the confirmation setting live under **Settings** in the web UI,
take effect on the next sync without a restart, and are listed under [The review
queue](#the-review-queue). Nothing about how transactions are matched is set
here — if you are looking for a knob because your bank behaves unusually, that
is where it is.

Settings are installation-wide rather than per bank. Two connected banks with
different habits share one tolerance and one pair of thresholds.

| Variable | Required | Default | Description |
|---|---|---|---|
| `BUDGET_BACKEND` | No | `actual` | Which budgeting backend to use: `actual` or `firefly` |
| `BUDGET_BACKEND_MIGRATE` | No | `false` | Required once when switching backends; discards the deduplication state and sync watermarks (see [Switching backends](#switching-backends)) |
| `ACTUAL_URL` | Yes (actual) | — | URL of your Actual Budget instance |
| `ACTUAL_PASSWORD` | Yes (actual) | — | Actual Budget server password |
| `ACTUAL_SYNC_ID` | Yes (actual) | — | Budget file sync ID |
| `ACTUAL_ACCOUNT` | No | — | Fixed Actual Budget account name for every bank. Leave unset to name each account after the bank and account it syncs; each bank can still be mapped to a different account via the web UI |
| `EB_APPLICATION_ID` | No | — | Enable Banking application ID (locks the field in the UI if set) |
| `EB_REQUEST_BALANCE_ACCESS` | No | `true` | Ask for balance access when authorising a bank. Set to `false` only if your bank rejects the authorisation request because of it — opening balances and drift detection are then unavailable |
| `SYNC_INTERVAL_HOURS` | No | `6` | How often to sync |
| `ACTUAL_INSECURE_TLS` | No | `false` | Skip TLS certificate verification when connecting to Actual Budget (useful for self-signed certs) |
| `FIREFLY_URL` | Yes (firefly) | — | URL of your Firefly III instance |
| `FIREFLY_TOKEN` | Yes (firefly) | — | Personal Access Token (Options > Profile > OAuth) |
| `FIREFLY_ACCOUNT` | No | — | Fixed asset account name for every bank. Leave unset to name each account after the bank and account it syncs (e.g. `Sparkasse Girokonto`), which keeps several connected banks apart |
| `FIREFLY_PENDING_TAG` | No | `pending` | Tag marking transactions the bank has not booked yet |
| `FIREFLY_APPLY_RULES` | No | `true` | Let Firefly's rule engine run on booked transactions. Never runs on pending ones |
| `FIREFLY_FIRE_WEBHOOKS` | No | `false` | Fire Firefly webhooks on import. Off by default so a backfill does not emit hundreds of events |
| `FIREFLY_INSECURE_TLS` | No | `false` | Skip TLS certificate verification when connecting to Firefly III |
| `ACCOUNT_HOLDER_NAME` | No | — | Your name(s) as they appear on transactions, comma-separated. Suppresses self-transfers from appearing as payees. |
| `WEB_ADDR` | No | `:8443` | Web UI listen address |
| `TRUSTED_PROXY` | No | `false` | Honour `X-Forwarded-Proto` / `X-Forwarded-Host`. Only enable behind a reverse proxy you control — see [Security](#security) |
| `EB_DUMP_RESPONSES` | No | — | Debug only: directory to write raw Enable Banking responses to. Contains real financial data — see `enablebanking/testdata/README.md` |
| `NOTIFY_EMAIL` | No | — | Email for sync failure alerts and session expiry warnings |
| `SMTP_HOST` | No | `smtp.gmail.com` | SMTP server |
| `SMTP_PORT` | No | `587` | SMTP port |
| `SMTP_USER` | No | — | SMTP username |
| `SMTP_PASS` | No | — | SMTP password |
| `OTLP_ENDPOINT` | No | — | OTLP gRPC endpoint as `host:port` (e.g. `collector:4317` — **no scheme**) for metrics, traces and logs |
| `PYROSCOPE_SERVER_ADDRESS` | No | — | Grafana Pyroscope URL for continuous profiling |
| `PYROSCOPE_BASIC_AUTH_USER` | No | — | Pyroscope basic auth username |
| `PYROSCOPE_BASIC_AUTH_PASSWORD` | No | — | Pyroscope basic auth password |

## Data volume

All state lives in a single Docker volume mounted at `/data`.

| Path | Description |
|---|---|
| `/data/bankingsync.db` | SQLite database — settings, bank accounts, sync log, sync state, transaction refs |
| `/data/tls.crt`, `/data/tls.key` | TLS certificate and key — auto-generated on first start |
| `/data/private.pem` | Enable Banking private key — optional alternative to uploading via the web UI |

To use your own TLS certificate, place it at `/data/tls.crt` and `/data/tls.key` before starting.

### What the database keeps, and for how long

Deduplication state — imported references, the pending map, held transactions —
is kept for **38 days**, which covers the rolling fetch window plus the match
window on both sides. Shorter and a transaction you deleted could fall out of
the state while still inside the fetch window, and the next sync would import it
again.

The decision log ages out differently, because it holds two different things.
An **unanswered** decision is a diagnostic: once the bank stops offering the
transaction nobody can check it either way, so it goes on the same 38 days. An
**answered** one is evidence, and evidence does not stop being true when the
transaction ages out — but everything saying *which* transaction it was stops
being needed at exactly that moment. At the 38-day mark the pending key, the
bank reference, the budget row id and the run id are cleared, and **both** dates
are coarsened to their month: a decision is made within hours of the transaction
reaching the feed, so leaving the decision timestamp at second precision would
locate the purchase more exactly than the transaction date ever did. What
remains — the comparison levels, the weight, the answer — can be counted and
cannot be read back to a purchase. What remains is deleted after **400 days**: one full annual cycle plus
a month, after which an observation is likelier to describe a format the bank has
since changed than the one it uses now.

Resetting the import state or removing a bank account clears all of it
immediately, regardless of age.

## Web UI

| Page | Path | Description |
|---|---|---|
| Setup | `/setup` | Upload PEM file and set Application ID |
| Connect | `/connect` | Browse banks by country, start OAuth, add a bank account |
| Pick Account | `/pick-account` | Choose a sub-account (shows IBAN, owner, currency), set the target Actual Budget account and sync start date |
| Status | `/status` | View accounts, sync history, trigger sync, test email, reset sync, renew or remove accounts |
| Review | `/review` | Transactions the matcher would not decide on its own, each with its candidates, their match probabilities and the levels behind them. Nothing here is in the budget until it is resolved. Also carries the one confirmation a sync may ask for about a decision it made alone — that half changes nothing and blocks nothing. |
| Matching | `/matching` | The matching parameters in force, the candidate your settled decisions support, and the three findings about the difference. Watching, promoting and reverting are all a person's decision and none happens on its own. |
| Test Email | `POST /test-email` | Send a test email to verify SMTP configuration |
| SBOM | `/sbom` | Browse the embedded CycloneDX SBOM — Go module and OS package inventory with licenses. Raw JSON download at `/sbom.json`. |
| Health | `/health` | Returns JSON with status (`ok`/`degraded`/`unhealthy`), version, connected accounts, expiring sessions, `reviews_open`, last sync info. HTTP 503 when unhealthy; `degraded` while any transaction is waiting for a decision. |

## Session renewal

Enable Banking sessions expire after roughly 180 days. bankingsync warns you by email (if configured) when a session is within 7 days of expiry. To renew, click **Renew** on the Status page and re-authorise with your bank. No data is lost — sync state and transaction history are preserved.

## Updating

```bash
docker compose pull && docker compose up -d
```

## Building from source

```bash
git clone https://github.com/RomanSpies/BankingSync.git
cd bankingsync
go build -o bankingsync .
go test ./...
```

Requires Go 1.25+. See [INSTALLATION.md](INSTALLATION.md) for details.

`go test ./...` covers everything that needs no server. The Firefly integration
tests sit behind a build tag, because they write to a real instance:

```bash
docker run -d --name ff-test -p 127.0.0.1:8080:8080 \
  -e APP_KEY=SomeRandomStringOf32CharsExactly -e DB_CONNECTION=sqlite \
  -e APP_URL=http://127.0.0.1:8080 -e TZ=America/New_York fireflyiii/core:latest
docker exec ff-test php artisan passport:client --personal --no-interaction --name=ci --provider=users

FIREFLY_LIVE_URL=http://127.0.0.1:8080 \
FIREFLY_LIVE_RUN_ID=$(date +%s) \
FIREFLY_LIVE_DESTRUCTIVE=yes \
go test -tags fireflylive ./...
```

The instance is written to and is meant to be thrown away afterwards. The suite
refuses to start against one holding an account it did not create, and fails
rather than skips when the environment is incomplete — a green run that tested
nothing is the outcome it exists to prevent. `TZ` is deliberately west of UTC:
Firefly renders dates in the server's own zone, and an eastern offset hides a
whole class of date bug.

`FIREFLY_LIVE_RUN_ID` has to be different for every run against the same
instance — it is what keeps account names apart. Exporting it once and running
twice makes the second run find the first one's accounts and fail, which is the
mechanism working rather than breaking.

## Metrics

> **Building dashboards or alerts?** [`docs/telemetry.md`](docs/telemetry.md) is
> the complete reference — every metric with its type, labels, label values,
> bucket boundaries and the conditions under which it is absent rather than zero;
> every span with its attributes; every log record; the OTLP-to-Prometheus naming
> rules; and worked PromQL for the questions worth asking. It is written to be
> handed to somebody who has never seen this codebase, and a test fails if an
> instrument is added without being described there. The table below is the
> summary.


Database errors are also logged as `store.operation_failed` with the statement
kind, the table and the cause. These records carry **no trace correlation**: the
store's methods take no `context.Context`, so a slow or failing query appears in
the metric and in the log but not as a span under the request that caused it.

When `OTLP_ENDPOINT` is set, bankingsync exports OpenTelemetry metrics via gRPC.

Everything that depends on which budget backend is in use carries a `backend`
label, so a dashboard can separate Actual from Firefly rather than adding them
together. Metrics about the bank side — the Enable Banking fetch, transactions
dropped while parsing — deliberately do not, because the backend has no bearing
on them.

### Counters

| Metric | Description |
|---|---|
| `bankingsync_sync_runs_total` | Sync cycles completed, labelled by status |
| `bankingsync_transactions_added_total` | New transactions imported |
| `bankingsync_transactions_confirmed_total` | Pending transactions promoted to booked |
| `bankingsync_near_miss_total` | Transactions created although an open row nearly matched, by `reason` |
| `bankingsync_review_problems_total` | Review and matching interactions that did not go through, by `op` (`list`, `resolve`, `confirm`, `matching`, `inquiry`) and `outcome` (`refused` when the program declined an answer made against a page that had gone stale, `failed` when it could not carry one out). A run of refusals means the page is being drawn from state that keeps moving underneath it |
| `bankingsync_match_reviews_total` | Transactions entering and leaving the review queue, by `bank`, `outcome` (`queued` when the matcher would not decide, `assigned` or `imported` when a person did) and `reason` (`ambiguous`, `uncertain`, or `decided` once a person has answered) |
| `bankingsync_match_review_choice_total` | Review answers by `choice`: `model_best` when the person merged into the candidate the matcher ranked first, `other_candidate` when they picked a different one, `created_new` when they called it new. This is the **only** direct measurement of the model's ranking anywhere in the program — everything else scores the probability it assigned, and this scores the order it put the candidates in. A rising `other_candidate` share is the matcher putting the wrong row first, which no calibration figure can see |
| `bankingsync_store_operations_total` | Database operations, by `kind` (select/insert/update/delete/commit), `table` and `result`. A lookup that matched nothing counts as `ok` — it is an ordinary answer, not a failure |
| `bankingsync_match_probability` | Probability of the strongest candidate considered for an incoming transaction, by `bank`, `outcome` (`adopted`/`created`/`held`) and `param_version` — the distribution the two thresholds cut through. Transactions with no candidate are not recorded. The `param_version` label is there because a distribution is only comparable with itself while the parameters that produced it have not moved; without it a promotion would look like a change in the banks. Promoting parameters therefore starts a new series, which is the intended behaviour and will break a dashboard that pins one label value |
| `bankingsync_match_margin` | How much total evidence a batch would lose if a chosen pairing were forbidden, by `bank` and `outcome`. Below one bit the arrangement had a free choice; above it the pairing stood on its own |
| `bankingsync_match_multiplicity_total` | Transactions settled onto one of several rows nothing could tell apart, by `bank`. The shape the reported multiplicity defect had, so the series to watch if it returns |
| `bankingsync_match_shadow_decisions_total` | Decisions made while a candidate parameter set was being watched, by `bank`, `backend`, `candidate` (the candidate's own parameter version) and `agreement` (`same`/`different`). The matching page reports how many a candidate would have changed; this reports when. `candidate` is on it because a counter does not reset when the watch moves on, and two candidates' tallies would otherwise add into one line describing neither |
| `bankingsync_match_inquiry_answers_total` | Answers to the one confirmation a sync may ask for, by `bank`, `outcome` (`adopted`/`created`) and `verdict` (`same_payment`, `different_payments`, `unknown`). A `unknown` share that stays high means the questions are unanswerable, not that the matcher is fine |
| `bankingsync_import_key_collisions_total` | Incoming transactions that would have shared an identity under an earlier import key — date and amount, without the payee — and been dropped as repeats of one another, by `bank`. Counts a defect that no longer happens, on purpose: it says whether a given feed ever reached it |
| `bankingsync_transactions_skipped_total` | Transactions skipped (already imported) |
| `bankingsync_transactions_dropped_total` | Transactions Enable Banking returned that failed to parse and were dropped. A defect: it degrades the run and triggers the alert email |
| `bankingsync_transactions_zero_amount_total` | Zero-amount transactions skipped on purpose, by `bank`. Some banks issue these routinely, so they are counted rather than treated as an error — a row without a direction has nothing to import and would match every other zero row |
| `bankingsync_rules_applied_total` | Rule actions applied to new transactions |
| `bankingsync_commit_errors_total` | Errors committing buffered changes. Actual only — Firefly is write-through and has nothing to flush |
| `bankingsync_write_errors_total` | Per-transaction write errors against the budget backend |
| `bankingsync_backend_requests_total` | Requests to the backend's API, by `route` and response `status`. Firefly only — Actual is not a REST backend |
| `bankingsync_backend_rate_limited_total` | Requests the backend rate limited and that were retried, by `route` |
| `bankingsync_backend_conflicts_total` | Writes that did not happen as intended, by `reason`: `transaction_gone` (deleted in the backend), `duplicate_rejected`, `user_split_group` (you split it, so bankingsync stopped touching it), `currency_mismatch`. Every one is a case where bankingsync deliberately does nothing, which is otherwise invisible |
| `bankingsync_balance_checks_total` | Balance comparisons concluded, by `bank` and outcome `state`. The drift gauge only shows the latest state; this shows how often a comparison was worth nothing |

### Histograms

| Metric | Description |
|---|---|
| `bankingsync_sync_duration_seconds` | Wall-clock duration of a full sync cycle |
| `bankingsync_fetch_duration_seconds` | Duration of the Enable Banking fetch, by `bank` |
| `bankingsync_budget_write_duration_seconds` | Time spent importing one bank account into the budget backend |
| `bankingsync_match_reference_probability` | What the model *would* have given a pair the bank's own reference settled, by `bank`, `backend` and `param_version`. Its accuracy against ground truth it did not provide. Deliberately a separate series from `bankingsync_match_probability`: these pairs never reached a threshold, so folding them in would make "how many decisions sit near a threshold" answer a different question. On a feed whose references are stable this is the **only** view of the matcher's accuracy, because the reference settles nearly everything before the model is consulted and `match_probability` then stays empty |
| `bankingsync_match_inquiry_bits` | What the one confirmation a sync asks for was expected to be worth, in bits of information about the matching parameters, by `bank` and `outcome`. An answer is one binary label, so it cannot be worth more than one bit and the buckets now run to it. **The range changed when the criterion did**: it used to be a second-order expansion, which at the variances a fresh installation carries returned thousandths, and the buckets ended at 0.5. Exact BALD saturates instead — measured on shipped parameters with an empty decision log every comparison is worth 0.84 to 0.96 bits, and against three hundred settled decisions the same comparisons are worth 0.00002 to 0.0015. A series pinned near one is an installation that knows nothing yet; one settling towards the bottom is saying there is nothing left worth asking about, and the setting can go back off |

### Gauges

| Metric | Description |
|---|---|
| `bankingsync_pending_transactions` | Pending transactions awaiting confirmation |
| `bankingsync_session_expiry_days` | Days until session expires |
| `bankingsync_balance_drift_cents` | Budget account total minus the bank's booked balance plus outstanding pendings, by `bank` and drift `state`. Accounts that have never been compared are not reported, so a missing series and a difference of zero cannot be confused |
| `bankingsync_store_duration_seconds` | Time spent in one database operation, by `kind` and `table`. Buckets start at 0.5 ms: a local SQLite file answers in microseconds, so the condition worth catching is not a slow query but a contended or failing database |
| `bankingsync_match_reviews_open` | Transactions held back for a person to decide, by `bank`. Unlike drift, zero **is** reported: nothing waiting is an unambiguous state, and an alert is easier to write against a series than against its absence |
| `bankingsync_match_labels_total` | Decisions settled by something other than the model, by `bank` and `source`: `review` for a person's answer, `reference` for a pair the bank's key confirmed, and `reference_model_agreed` for whether the matcher would have got there on its own |
| `bankingsync_match_brier_score` | Brier score against settled decisions, by `component`. Six of them, not three: `score`, `reliability`, `resolution`, `uncertainty`, `within_bin_variance`, `within_bin_covariance` and `generalised_resolution`. Murphy's three-way identity only holds when you stratify on the distinct forecast values; this bins by width, so two further terms appear and `reliability − resolution + uncertainty` is out by an amount that depends on nothing but the bin count. Plot `reliability − generalised_resolution + uncertainty` and it closes against `score` exactly. Reported only above fifty settled decisions |
| `bankingsync_match_calibration_error` | Expected calibration error: the average gap between what the matcher claimed and what happened. Reported only above **three hundred** settled decisions, which is a higher bar than the other calibration series and deliberately so. The binned estimator is biased upwards and the bias does not vanish on a correct model — measured on forecasts calibrated by construction it reports about 0.14 at fifty observations and 0.064 at a hundred and twenty — so below that floor there is no series rather than a small number. An alarm that fires on a correct model is worse than no alarm |
| `bankingsync_match_level_weight` | What a comparison level is currently worth in bits — log₂(m/u) — by `field` and `level`, read from the parameters actually deciding. Constant while the shipped ones are in force, which is why it is here: promoting a fitted set moves it, and that is the event worth watching for |
| `bankingsync_match_level_observations` | How many settled decisions actually reached each level, by `field`, `side` (`m`/`u`) and `level`. The counterpart to `match_level_weight`: that one says what a level is worth, this says whether anybody has ever seen one. A level whose `m` side is empty is one the refit holds at the ratio it shipped with — a stated claim rather than a measurement — so this is the series that says how much of the model is still a guess |
| `bankingsync_match_policy` | The matching policy in force, by `setting`: `auto_probability`, `review_probability`, `overlap` and `tolerance_percent`. Without it a shift in any other matching series has to be correlated against a log line to be explained, and a threshold move is the commonest reason for one |
| `bankingsync_match_calibration_coefficient` | The Platt rescaling in force, by `coefficient`: `slope` (A) and `intercept` (B), in bits. `A = 1` with `B = 0` is no rescaling at all. This is the only place a calibration that quietly fell back to the identity is distinguishable from one that was never fitted, and the only place a slope that ran away is visible before it shows up in the Brier score |
| `bankingsync_match_gate` | What the promotion gate last concluded, by `figure`: `labelled`, `holdout`, `base_brier`, `trial_brier`, and — only when there was something to test — `skill_percent`, `p_value`, `significance_level` and `statistic`. The Brier skill score is how much of the incumbent's loss the candidate removes; the p-value is a Diebold-Mariano test on the paired loss differential, and `significance_level` is the bar it had to clear after correcting for how many looks the corpus has afforded. **Read from the last real evaluation rather than provoking one**, because reaching a verdict refits the linkage and fits Platt twice — fine on a page load, not fine every thirty seconds. A flat line therefore means either that nothing has changed or that nobody has opened the matching page |
| `bankingsync_match_gate_check` | Each promotion check's standing, by `check` and `status`: one where it holds that status and zero where it does not. They fail for unrelated reasons and want unrelated responses — a calibration check that will not open wants more labels, an anchor check that will not open wants somebody to look at what the candidate would do to a documented case. `check="overall"` with `status="promotable"` is the whole gate |

## Traces

Spans go to the same `OTLP_ENDPOINT` as the metrics. The tree is shallow on
purpose — a span per step that can be slow for its own reasons, not a span per
function.

```
sync.run
├── budget.ensure_connection
├── enable_banking.fetch_transactions        one per bank account
├── import.transactions_batch                one per bank account
│   └── match.reconcile_batch                one per batch placed together
└── rules.apply                              Actual only
```

`match.reconcile_batch` is the one worth knowing about. It is the only step in an
import whose cost is not proportional to the transactions in it: a batch is
weighed against every open row in a fortnight and then arranged one-to-one, so
the work grows with the account's traffic rather than with the feed's. An import
that has become slow on a busy account looks, from every other signal, exactly
like one that is slow for any other reason. It carries `incoming`,
`candidates_weighed`, `adopted`, `held`, `created`, `weakest_best_probability`,
`param_version` and whether a `shadow` set was being evaluated alongside.

The HTTP server traces every request, and one handler has a child span:

```
GET /matching
└── match.promotion_status
```

That one exists because it is the most expensive thing a request in this program
does, and nothing about the page says so. Reaching a verdict refits the linkage,
fits a Platt calibration, runs six anchor cases through the real decision
function and takes a significance test over the held-back third — twice, once for
the candidate and once for what is in force. It is recomputed from the corpus on
every page load on purpose, because a stored verdict would describe parameters
proposed a month ago. The span carries `labelled_decisions`, `holdout`,
`candidate`, `promotable`, the `skill_percent`, `p_value` and
`significance_level` when there was enough to test, and one attribute per gate
check.

Enable Banking and both budget backends trace their own calls
(`enablebanking.fetch_page`, `actual.commit`, `firefly.*`), so a slow sync can be
attributed to the bank, the backend or the matching without guessing.

**Database work is not traced.** The store's methods take no `context.Context`,
so a slow or failing query appears in `bankingsync_store_duration_seconds` and in
the `store.operation_failed` log record, but not as a span under the request that
caused it.

## Upgrading to version 4

The upgrade itself needs nothing: pull and restart. New tables and columns are
created on open, the state left by an earlier version is read as it stands, and
no transaction is re-imported. Five things are worth knowing, and the first two
change what the matcher does.

**Any parameter set you had promoted is discarded, once, on first open.** A
promoted set is the fitted numbers themselves rather than a recipe — it is loaded
into the live policy on every run and nothing re-derives it — and the sets fitted
before this release came off a decision log with three defects in it: labels
scored against rows the merge had already rewritten, labels harvested from a key
built out of the model's own comparison fields, and a candidate count that was
constant. No column records which corpus produced a set, so a contaminated one
cannot be told from a sound one afterwards. The shipped parameters take over, the
discard is logged rather than silent, and a refit becomes available again once
enough sound labels have accumulated.

**The shipped parameters changed, so `param_version` changes with them.** Every
series carrying that label starts fresh. A truncated payee was worth more than a
verbatim one, which meant a pair agreeing in full could be held for review while
the cut-off spelling of the same pair merged unasked; the term-frequency floor
scaled with statement length, so it loosened as an account accumulated history;
and the refit's prior concentration was a number with nothing behind it. The
sensitivity table in `budget/sensitivity.md` is regenerated against the new set.
Neither threshold moved.

**A feed whose bank supplies no reference now produces no labels.** Confirmations
found through the fallback identity — date, amount and normalised payee — are
still recorded and still counted, under
`bankingsync_match_labels_total{source="fallback_key"}`, because a merge the key
caught and the matcher would have missed is worth knowing about. Their outcome is
left unanswered, because a pair selected on the very fields the model compares
agrees on them by construction, and fitting to it would ask the model to confirm
the rule that picked it. On such a feed the review queue and the optional
confirmations are the only sources of evidence.

**Dashboards will need work.** Seven series are new — `bankingsync_match_gate`,
`bankingsync_match_gate_check`, `bankingsync_match_level_observations`,
`bankingsync_match_calibration_coefficient`, `bankingsync_match_policy`,
`bankingsync_match_review_choice_total`, and `bankingsync_match_reference_probability`
— and three changed shape. `bankingsync_match_brier_score` publishes six
components rather than three, because the three-component identity only holds
when you stratify on distinct forecast values and this bins by width;
`bankingsync_match_calibration_error` refuses to answer below three hundred
settled decisions, because the binned estimator reports about 0.064 on a model
that is perfectly calibrated at the corpus sizes this program reaches; and
`bankingsync_match_inquiry_bits` has new buckets, because the criterion it
measures is now bounded by one bit and the old buckets ended at a half. Three
spans are new: `match.reconcile_batch`, `match.decide` and
`match.promotion_status`. [docs/telemetry.md](docs/telemetry.md) is the full
reference, and it is checked against the source by a test.

**`ambiguous` means something different, for the second release running.** It was
the gap between the best candidate and the runner-up; it is now what the whole
batch would lose if the chosen pairing were forbidden. The section on the review
queue above explains why, and where the counter moved to.

Transactions held for review by an earlier version are still there and still
decidable. Their candidates are recomputed on the page, so they are judged by the
current parameters rather than the ones that held them. The comparison levels
recorded against them at the time are only ever displayed and logged, and an old
row may name a level this version no longer has. Nothing reads those names back.

## Migrating from a previous version

If upgrading from a version that used `state.json`:

1. Keep your `/data` volume in place
2. Pull and restart: `docker compose pull && docker compose up -d`
3. `state.json` is automatically migrated into `bankingsync.db` and renamed to `state.json.migrated`
4. If `/data/private.pem` exists, it is detected automatically
5. Re-authorise your bank from the Connect page (sync state and transaction refs are preserved — no duplicates)

## License

GNU Affero General Public License v3.0 — see [LICENSE](LICENSE).

See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for dependency licenses.
