# Telemetry reference

Everything bankingsync emits, in enough detail to build recording rules, alerts
and dashboards from without reading the source.

Written to be handed to somebody — or something — that has never seen this
codebase. Where a number matters it is given, and where a series behaves in a way
that would mislead a naive query that is said rather than left to be discovered.

---

## 1. Transport and configuration

One environment variable turns all of it on:

| Variable | Effect |
|---|---|
| `OTLP_ENDPOINT` | `host:port` of an OTLP **gRPC** collector, **no scheme**, e.g. `collector:4317`. Unset means metrics, traces and logs are all disabled and the program runs normally. |
| `PYROSCOPE_SERVER_ADDRESS` | Continuous profiling, separate from OTLP. Optional. |
| `PYROSCOPE_BASIC_AUTH_USER` / `_PASSWORD` | Credentials for the above. |

Metrics, traces and logs go to the **same endpoint** over gRPC with
`WithInsecure()` — there is no TLS option, so the collector is expected to be on
a trusted network or a sidecar.

Facts a rule or dashboard depends on:

- **Collection interval: 30 seconds.** A `PeriodicReader`, so every observable
  gauge is evaluated on that cadence and no faster. Do not write rules expecting
  sub-30s resolution.
- **Temporality: cumulative.** The exporters are constructed without a
  temporality selector, so the Go SDK default applies. Counters and histograms
  arrive as monotonic cumulative series, which is what `rate()` and
  `histogram_quantile()` expect and what remote-write wants.
- **Traces are not sampled.** No sampler is configured, so the SDK default
  `ParentBased(AlwaysSample)` applies and **every span is exported**. See
  §6 for what that means for volume.
- **Resource attributes:** `service.name="bankingsync"`,
  `service.version=<build version>`, plus the SDK defaults
  (`telemetry.sdk.name`, `telemetry.sdk.language`, `telemetry.sdk.version`).
  Under remote-write these land on `target_info`, not on the series.

Instrumentation scopes, which become `otel_scope_name` if the collector is
configured to keep them:

| Scope | What it covers |
|---|---|
| `bankingsync` | the sync loop, the matcher, the model |
| `bankingsync/web` | the HTTP server and the review pages |
| `bankingsync/store` | SQLite |
| `bankingsync/firefly` | the Firefly III backend |
| `bankingsync/actual` | the Actual backend (traces only) |
| `bankingsync/enablebanking` | the bank API (traces only) |

---

## 2. Names in Prometheus and Mimir

The collector's `prometheusremotewrite` exporter does the translation. With
default settings:

- Metric names pass through unchanged. Every name here already ends in `_total`
  where it is a counter and already carries a unit suffix where it has one, so
  the exporter's `add_metric_suffixes` adds nothing further. **If you disable
  `add_metric_suffixes` nothing changes; if you enable unit suffixes on a
  collector that also rewrites, check for `_seconds_seconds`.**
- Histograms become three series: `<name>_bucket{le=...}`, `<name>_sum`,
  `<name>_count`.
- Observable gauges become plain gauges under their own name.
- Resource attributes become `target_info`; join with
  `on (job, instance) group_left(...) target_info` if you want
  `service_version` on a panel.

**There is exactly one instance per installation.** bankingsync is a
single-process, single-node program with a local SQLite file. Rules do not need
`sum by (instance)` for correctness, but keeping `instance` in an alert's labels
is still useful if somebody runs two.

---

## 3. Metrics — the model

These are the ones that answer "is the matcher working and is it getting
better". They are the reason this document exists.

### 3.1 Decision distribution

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `bankingsync_match_probability` | histogram | `bank`, `backend`, `outcome`, `param_version` | Probability of the strongest candidate for one incoming transaction. **The distribution the two thresholds cut through** — this is what makes the thresholds settable. Buckets: `0.1, 0.25, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95, 0.99`. Not recorded when the window held no candidate at all, deliberately: a zero would pile a spike at the bottom meaning "nothing was there" rather than "a very unlikely match". |
| `bankingsync_match_margin` | histogram | `bank`, `backend`, `outcome` | How much total evidence the batch would lose if the chosen pairing were forbidden. Buckets: `0.25, 0.5, 1, 2, 4, 8, 16` **bits**. Below 1 bit the arrangement had a free choice and the case goes to a person; above it the pairing stood on its own. Not recorded when nothing was paired (the margin is then infinite, which is not a measurement). |
| `bankingsync_match_reference_probability` | histogram | `bank`, `backend`, `param_version` | What the model *would* have said about a pair the bank's own reference had already settled. Same buckets as `match_probability`. **Deliberately a separate series:** these pairs never reached a threshold, so folding them in would make "how many decisions sit near a threshold" answer a different question. On a feed with stable references this is the **only** view of the matcher's accuracy, because `match_probability` then stays nearly empty. |

`outcome` ∈ `adopted`, `created`, `held`.

**`param_version` is a 12-hex-character hash** of the whole parameter set —
every level probability, both thresholds, the margin, the overlap, the tolerance,
the payee prefixes and the calibration coefficients. Promoting parameters, or an
operator moving a threshold, **starts a new series**. That is intended: a
distribution is only comparable with itself while what produced it has not moved.
It also means a dashboard that pins a `param_version` will go blank after a
promotion, and that a naive `sum by (le)` across versions mixes two populations.

### 3.2 Calibration and discrimination

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `bankingsync_match_brier_score` | gauge | `component`, `backend` | Mean squared error of the probabilities against settled decisions, decomposed. **Seven components**, see below. Reported only above **50** settled decisions; absent below that. |
| `bankingsync_match_calibration_error` | gauge | `backend` | Expected calibration error. Reported only above **300** settled decisions — a higher bar than the Brier score and deliberately so, see below. |
| `bankingsync_match_calibration_coefficient` | gauge | `coefficient` | The Platt rescaling in force. `coefficient` ∈ `slope`, `intercept`. `slope=1, intercept=0` is no rescaling. |

`component` ∈ `score`, `reliability`, `resolution`, `uncertainty`,
`within_bin_variance`, `within_bin_covariance`, `generalised_resolution`.

**Do not plot `reliability - resolution + uncertainty` against `score`.** It
will not close. Murphy's three-way identity holds only when you stratify on the
distinct forecast values; this bins by equal width and uses each bin's mean, so
two further terms appear. The identity that does hold, at any bin count:

```
score = reliability - generalised_resolution + uncertainty
```

equivalently `reliability - resolution + uncertainty + within_bin_variance -
within_bin_covariance`. Measured on a real corpus the three-way form was out by
0.0023 where the five-way form closed exactly.

**Why the calibration error needs 300 and the Brier score needs 50.** The binned
ECE estimator is biased upwards and the bias does not vanish on a *correct*
model. Measured on forecasts calibrated by construction, so that the true error
is zero, ten equal-mass bins report about **0.14 at fifty observations, 0.064 at
a hundred and twenty, 0.041 at three hundred**. An alert on ECE below a few
hundred labels fires on correctness. Above the floor the series is honest but
still biased upward at small N — treat a *trend* as meaningful and an absolute
value below about 0.03 as indistinguishable from zero.

**Reading the slope.** `coefficient="slope"` far from 1 with `intercept` far from
0 is a fitted calibration doing real work. A slope of exactly 1 with an intercept
of exactly 0 means either that nothing was ever fitted or that a fit was attempted
and gave up — the two are the same object, and only the promotion trace
(§6, `fit.platt_outcome`) tells them apart.

### 3.3 What the model knows

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `bankingsync_match_level_weight` | gauge | `field`, `level` | What a comparison level is currently worth, in **bits**: log₂(m/u), read from the parameters actually deciding. Constant while the shipped parameters are in force — which is the point: a promotion moves it, and that is the event worth watching. |
| `bankingsync_match_level_observations` | gauge | `field`, `side`, `level` | How many settled decisions actually reached each level. **The counterpart to the above**: that one says what a level is worth and says the same thing whether an installation has settled a thousand decisions or none; this says whether anybody has ever seen one. |
| `bankingsync_match_policy` | gauge | `setting` | The policy in force. `setting` ∈ `auto_probability`, `review_probability`, `overlap`, `tolerance_percent`. |

`field` ∈ `payee`, `amount`, `date`. `side` ∈ `m`, `u`.

`level` by field — **seventeen in total**, and a full dashboard row should show
all of them:

- `payee`: `exact`, `truncated`, `fuzzy`, `subset`, `missing`, `conflict`, `none`
- `amount`: `exact`, `higher_within`, `lower_within`, `outside_higher`, `outside_lower`
- `date`: `same`, `after_near`, `before_near`, `after_far`, `before_far`

`match_level_observations` reports **every level on every collection, zeros
included**. That is deliberate: if it only reported levels that had been seen, a
missing series and no evidence would look the same on a graph.

A level whose `side="m"` count is zero is one the refit **holds at the weight it
shipped with** rather than estimating — a stated claim, not a measurement. The
count of those is published directly as
`bankingsync_match_gate{figure="levels_held_at_prior"}`.

`match_policy` exists so that a step in any other matching series can be
explained without joining it to a log line. `overlap` and both thresholds are
operator settings; `tolerance_percent` is in percent, the rest are fractions.

### 3.4 The promotion gate

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `bankingsync_match_gate` | gauge | `figure` | What the gate last concluded. |
| `bankingsync_match_gate_check` | gauge | `check`, `status` | 1 where the check holds that status, 0 where it does not. |

`figure` ∈ `labelled`, `holdout`, `base_brier`, `trial_brier`,
`levels_held_at_prior`, and — **only when there was something to test** —
`skill_percent`, `p_value`, `significance_level`, `statistic`.

The four conditional figures are **absent, not zero**, when the corpus is too
thin to compare anything. A p-value of zero would read as overwhelming
significance, so it is not published. Write `absent()` rules accordingly.

- `skill_percent` is the Brier skill score against the parameters in force,
  ×100: how much of the incumbent's loss the candidate removes. Positive is
  better.
- `p_value` is a **Diebold-Mariano** test on the paired loss differential with
  the Harvey-Leybourne-Newbold small-sample correction, one-sided in the
  "candidate is better" direction.
- `significance_level` is the bar it had to clear: `0.05` divided by how many
  effectively independent looks the corpus has afforded, capped at four. So it
  takes exactly one of four values — **0.05, 0.025, 0.01667, 0.0125** — and never
  anything lower. The look count rises at 100, 200 and 400 settled decisions and
  stops there.
- `levels_held_at_prior` is out of seventeen. A candidate that scores well with
  half its levels in that state is scoring well on the shipped table.

`check` ∈ `anchor cases`, `calibration`, `changed decisions`, `overall`.
`status` ∈ `passed`, `failed`, `unavailable`, `for a person`, and — for
`check="overall"` only — `promotable`.

**This gauge is read from the last real evaluation and does not provoke one.**
Reaching a verdict refits the linkage and fits a Platt calibration twice, runs
six anchor cases through the real decision function and takes a significance
test. That is affordable on a page load and would not be affordable every thirty
seconds. So the series **only moves when somebody opens `/matching`**, and the
whole gauge is absent until that has happened once. A flat line means either that
nothing changed or that nobody looked; there is no series that tells those apart,
and a dashboard should say so in a panel description rather than imply freshness.

### 3.5 Labels, review and the question the program asks

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `bankingsync_match_labels_total` | counter | `bank`, `source`, `agreed` | Decisions settled by something other than the model — the only observations the model did not produce itself. |
| `bankingsync_match_reviews_total` | counter | `bank`, `backend`, `outcome`, `reason` | Transactions entering and leaving the review queue. |
| `bankingsync_match_review_choice_total` | counter | `bank`, `backend`, `choice` | **Whether the person merged into the row the model ranked first.** |
| `bankingsync_match_reviews_open` | gauge | `bank` | Transactions waiting for a person right now. |
| `bankingsync_match_inquiry_bits` | histogram | `bank`, `outcome` | What the one confirmation a sync may ask for was expected to be worth. |
| `bankingsync_match_inquiry_answers_total` | counter | `bank`, `outcome`, `verdict` | The answers to those. |
| `bankingsync_match_shadow_decisions_total` | counter | `bank`, `backend`, `candidate`, `agreement` | Decisions made while a candidate parameter set was being watched. |
| `bankingsync_match_multiplicity_total` | counter | `bank`, `backend` | Transactions settled onto one of several rows nothing could tell apart. |
| `bankingsync_near_miss_total` | counter | `bank`, `backend`, `reason` | Transactions created although an open row nearly matched. |

Label values:

- `source` ∈ `review`, `reference`, `fallback_key`, `reference_unscorable`,
  `reference_model_agreed`, `fallback_key_model_agreed`. The `_model_agreed`
  variants carry `agreed` ∈ `yes`, `no` and say whether the matcher would have
  reached the same pairing on its own.
- `outcome` on `match_reviews_total` ∈ `queued`, `assigned`, `imported`;
  `reason` ∈ `ambiguous`, `uncertain` (when queued), `decided` (once answered).
  `ambiguous` means the arrangement had a free choice — the margin was under a
  bit; `uncertain` means the probability landed in the band.
- `choice` ∈ `model_best`, `other_candidate`, `created_new`.
- `verdict` ∈ `same_payment`, `different_payments`, `unknown`.
- `agreement` ∈ `same`, `different`.
- `reason` on `near_miss_total` ∈ `ambiguous`, `payee`, `amount`, `date`.
- `candidate` is the watched set's own 12-hex `param_version`. It is on the
  series because a counter does not reset when the watch moves on, and two
  candidates' tallies would otherwise add into one line describing neither.

**`match_review_choice_total` is the only direct measurement of the model's
ranking anywhere in the program.** Everything else scores the probability it
assigned; this scores the order it put the candidates in. A rising
`other_candidate` share is the matcher putting the wrong row first, and no
calibration figure can see that.

**`match_inquiry_bits` buckets:** `0.00001, 0.0001, 0.001, 0.01, 0.05, 0.1, 0.25,
0.5, 0.75, 0.9, 0.99`. The value is bits of expected information about the
parameters and **cannot exceed 1**, because an answer is one binary label.
Measured on shipped parameters with an empty decision log every comparison is
worth **0.84 to 0.96** bits; against three hundred settled decisions the same
comparisons are worth **0.00002 to 0.0015**. So a series pinned near the top is a
fresh installation that knows nothing, and one settling into the bottom buckets
is saying there is nothing left worth asking about and the setting can go back
off.

A high and stable `verdict="unknown"` share means the questions are
unanswerable, not that the matcher is fine.

---

## 4. Metrics — sync, backends and storage

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `bankingsync_sync_runs_total` | counter | `backend`, `status` | `status` ∈ `success`, `degraded`, `error`, `fetch_error`, `no_session`. |
| `bankingsync_sync_duration_seconds` | histogram | `backend` | Buckets `1, 2, 5, 10, 30, 60, 120`. |
| `bankingsync_fetch_duration_seconds` | histogram | `bank` | Buckets `0.1, 0.5, 1, 2, 5, 10, 30`. |
| `bankingsync_budget_write_duration_seconds` | histogram | `backend` | One bank account's import. Buckets `0.01, 0.05, 0.1, 0.5, 1, 5, 15, 60, 300`. |
| `bankingsync_transactions_added_total` | counter | `backend` | |
| `bankingsync_transactions_confirmed_total` | counter | `backend` | Pending promoted to booked. |
| `bankingsync_transactions_skipped_total` | counter | `backend` | Already imported. |
| `bankingsync_transactions_dropped_total` | counter | `bank` | Failed to parse. **Any value above zero is a bug or a bank change.** |
| `bankingsync_transactions_zero_amount_total` | counter | `bank` | Skipped on purpose, not an error. |
| `bankingsync_import_key_collisions_total` | counter | `bank` | Transactions that *would* have collided under the pre-v3 import key. Counts a defect that no longer happens; a rising series says a feed reaches the shape that used to lose data. |
| `bankingsync_rules_applied_total` | counter | `backend` | Actual only — Firefly runs rules server-side. |
| `bankingsync_commit_errors_total` | counter | `backend` | |
| `bankingsync_write_errors_total` | counter | `backend` | Per-transaction. |
| `bankingsync_balance_checks_total` | counter | `bank`, `backend`, `state` | `state` ∈ `ok`, `drift`, `""` (unknown). |
| `bankingsync_balance_drift_cents` | gauge | `bank`, `state` | Budget total minus the bank's booked balance plus outstanding pendings. **Accounts never compared are not reported**, so a missing series and a drift of zero are different things. |
| `bankingsync_pending_transactions` | gauge | — | |
| `bankingsync_session_expiry_days` | gauge | — | **Absent until known.** |
| `bankingsync_store_operations_total` | counter | `kind`, `table`, `result` | `kind` ∈ `select`, `insert`, `update`, `delete`, `replace`, `begin`, `commit`, `create`, `pragma`, `vacuum`, `analyze`, `other`. `result` ∈ `ok`, `error`. A query matching no rows is `ok`, not an error. |
| `bankingsync_store_duration_seconds` | histogram | `kind`, `table`, `result` | Buckets `0.0005, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5`. A local SQLite file answers in microseconds; the condition worth catching is a contended or failing database, not a slow query. |
| `bankingsync_backend_requests_total` | counter | `backend`, `method`, `route` | Firefly only. |
| `bankingsync_backend_rate_limited_total` | counter | `backend` | Firefly only. |
| `bankingsync_backend_conflicts_total` | counter | `backend` | Firefly only. |
| `bankingsync_review_problems_total` | counter | `op`, `outcome` | `op` ∈ `list`, `resolve`, `confirm`, `inquiry`, `matching`. `outcome` ∈ `refused`, `failed`. **A refusal is the program working** — a stale page was rejected; a failure is not. The ratio is the interesting figure, which is why they share a series. |

`backend` ∈ `actual`, `firefly`. `bank` is the operator's label for a bank
account and is **unbounded in principle** — one series per connected account.

---

## 5. Cardinality

Nothing here is high-cardinality by web-service standards, but two labels grow:

- **`param_version` and `candidate`** are 12-hex hashes. A new one appears on
  every promotion and on every threshold change. Over years an installation might
  accumulate tens; a churning one that keeps moving thresholds could accumulate
  more. They are on `match_probability`, `match_reference_probability` and
  `match_shadow_decisions_total` only.
- **`bank`** grows with connected accounts — single digits.

`match_level_observations` is fixed at **34 series** (17 levels × 2 sides) and
`match_level_weight` at 17. `match_gate_check` is 4 checks × 4 statuses + 1.

---

## 6. Traces

Every span is exported; there is no sampler. Volume is dominated by
`match.decide`, which is **one span per incoming transaction that reached the
model** — so a steady state of a few dozen a day, and a first-run backfill of
thirty days in one burst. A years-long backfill would be thousands of spans in
one trace tree. If that matters, sample at the collector on
`name="match.decide"`; nothing else in the program is high-volume.

### Sync

```
sync.run
├── budget.ensure_connection
├── enable_banking.fetch_transactions       one per bank account
├── import.transactions_batch               one per bank account
│   └── match.reconcile_batch               one per batch placed together
│       └── match.decide                    one per incoming transaction
└── rules.apply                             Actual only
```

**`match.reconcile_batch`** — the only step in an import whose cost is not
proportional to the transactions in it. A batch is weighed against every open row
in a fortnight and then arranged one-to-one, so the work grows with the account's
traffic rather than with the feed's.

| Attribute | Meaning |
|---|---|
| `bank`, `param_version`, `shadow` | which account, which parameters, whether a candidate set was being evaluated alongside |
| `incoming` | transactions in the batch |
| `modelled` | how many actually reached the model (fewer, when a bank reference settled some first) |
| `candidates_weighed` | **the work** — rows compared across every window |
| `assess_ms` | reading and comparing the windows; carries a backend call, grows with the account's history |
| `arrange_ms` | the Hungarian assignment; grows with the batch |
| `shadow_ms` | the whole of the first two again against a watched candidate set, zero when none is being watched |
| `adopted`, `held`, `created` | what the batch concluded |

An import that has become slow is one of those three phases and the split says
which.

**`match.decide`** — one decision, in enough detail to explain it rather than
only state it. It is a **record, not a timing**: it is emitted once the decision
has been made, so its duration is nothing and it exists to be searched by
attribute.

| Attribute | Meaning |
|---|---|
| `outcome` | `adopted` / `held` / `created` / `unsettled` |
| `reason` | the rule that settled it, in words — `inside the review band`, `below the review threshold`, `over the automatic threshold but the arrangement had a free choice`, `over the automatic threshold and clear of the alternatives`, `nothing in the window survived the hard rules` |
| `window_rows`, `adoptable`, `plausible` | the fortnight's rows, how many survived the hard rules, and how many the prior was taken over. **The three differ, and where they differ is often the answer.** |
| `weight`, `probability` | the strongest candidate's match weight in bits and its calibrated probability |
| `payee_level`, `amount_level`, `date_level` | the comparison that produced it |
| `payee_frequency` | this account's frequency for that payee, when one was measured |
| `bits.prior`, `bits.payee`, `bits.amount`, `bits.date` | **the weight term by term.** They sum to `weight`. The term that dominates is the field that decided. |
| `level.prior`, `level.payee`, `level.amount`, `level.date` | the level each term came from; `level.prior` reads `1 of n` |
| `auto_probability`, `review_probability`, `margin_bits` | the thresholds this decision was judged against, so a span read a month later can still be interpreted |
| `margin` | the arrangement's answer to whether there was a real alternative; absent when nothing was paired |
| `runner_up_weight`, `runner_up_probability`, `window_gap` | the second-best candidate and the gap to it, in bits |
| `interchangeable` | rows nothing could tell apart, when more than one |
| `chosen_id` | the row merged into |
| `shadow_outcome` | what a watched candidate set would have done |
| `empty_window` | true when there was no candidate at all |

The query that finds every case the margin rule caught:
`outcome="held" && probability >= 0.9`. The query that finds a bank whose payee
field is carrying the whole model: `bits.payee > 8`.

### The matching page

```
GET /matching
└── match.promotion_status
```

The most expensive thing an HTTP request in this program does, and nothing about
the page says so — see §3.4.

| Attribute | Meaning |
|---|---|
| `labelled_decisions`, `param_version` | the corpus and what is in force |
| `candidate`, `promotable`, `watching` | the proposed set, whether the gate is open, whether it is being shadowed |
| `holdout` | decisions held back from fitting and used to judge |
| `skill_percent`, `p_value`, `significance_level`, `base_brier`, `trial_brier` | the comparison; **absent when there was nothing to test** |
| `check.<name>` | one per gate check, carrying its status |
| `fit.alpha` | the Dirichlet concentration used |
| `fit.labelled`, `fit.sampled_u` | evidence in, m side and u side. They differ by orders of magnitude and that asymmetry is the design |
| `fit.levels_held_at_prior` | of seventeen, how many came back still carrying their shipped weight |
| `fit.platt_outcome` | `converged`, `no further decrease`, `one outcome only`, `indefinite hessian`, `unusable coefficients`, `iteration limit` |
| `fit.platt_iterations`, `fit.platt_positive`, `fit.platt_negative` | how the fit went and what it saw |
| `fit.platt_slope`, `fit.platt_intercept` | the coefficients reached |

**`fit.platt_outcome` is the only place a calibration that gave up is
distinguishable from one that was never fitted.** Both produce slope 1,
intercept 0.

### Elsewhere

Every HTTP request is traced twice over: `HTTP <METHOD>` from the server
middleware and `<METHOD> <path>` from the handler.

Enable Banking and both budget backends trace their own calls, so a slow sync can
be attributed to the bank, the backend or the matching without guessing:

| Scope | Spans |
|---|---|
| `bankingsync/enablebanking` | `enablebanking.fetch_page`, `enablebanking.fetch_balances`, `enablebanking.get_aspsps`, `enablebanking.start_auth`, `enablebanking.complete_auth` |
| `bankingsync/actual` | `actual.init`, `actual.login`, `actual.download_budget`, `actual.sync`, `actual.sync_sync`, `actual.commit` |
| `bankingsync/firefly` | `firefly.list_transactions`, `firefly.find_by_external_ref`, `firefly.create_transaction`, `firefly.update_transaction`, `firefly.get_or_create_account`, `firefly.account_balance`, `firefly.set_opening_balance` |
| `bankingsync` | `email.send`, `update.check`, `update.fetch_dockerhub` |

The two backends are not instrumented alike: Firefly is a REST API and every call
is a request worth timing, so each has a span; Actual is a local sync engine and
the spans are around its lifecycle rather than around each row.

**Database work is not traced.** The store's methods take no `context.Context`,
so a slow or failing query appears in `bankingsync_store_duration_seconds` and in
the `store.operation_failed` log record, but never as a span under the request
that caused it. This is a known and stated limitation, not an oversight.

---

## 7. Logs

Structured records over OTLP. The body is a stable dotted name — `<area>.<event>`
or `<area>.<verb>.<result>` — and the detail is in attributes, so a log pipeline
should key on the body and never parse it.

Severities are used deliberately and are worth trusting: **Error** is the program
not working, **Warn** is the program working and declining to do something, and
**Info** is a state change worth annotating a graph with. In particular a
`review.refused` is a Warn because a stale page was correctly rejected, and a
`match.held_for_review` is a Warn because a transaction is now in no budget until
somebody acts — neither is a fault.

Every record the program can emit, by area:

| Area | Records |
|---|---|
| `sync` | `sync.started` (I), `sync.finished` (I) |
| `fetch_transactions` | `.completed` (I), `.failed` (E) |
| `import` | `import.batch.completed` (I) |
| `transaction` / `transactions` | `transaction.parse.failed` (W), `transactions.dropped` (E) |
| `match` | `match.held_for_review` (W), `match.near_miss` (W), `match.hold_failed` (E), `match.review_resolved` (I), `match.inquiry_raised` (I), `match.inquiry_answered` (I), `match.trial_watched` (I), `match.trial_promoted` (I), `match.trial_reverted` (I), `match.trial_dropped` (I), `match.decision_not_recorded` (W), `match.inquiry_not_recorded` (W), `match.usample_not_recorded` (W) |
| `review` | `review.refused` (W), `review.failed` (E) |
| `balance` | `balance.no_usable_type` (W), `balance.moved_during_run` (W), `balance.unavailable` (E) |
| `drift` | `drift.detected` (W), `drift.record_failed` (E), `drift.total_failed` (E) |
| `opening_balance` | `.set` (I), `.deferred` (W), `.write_failed` (E), `.not_recorded_locally` (E) |
| `budget` | `budget.ensure_connection.failed` (E), `budget.commit.failed_in_sync` (E), `budget.commit.failed_in_review` (E) |
| `rules` | `rules.applied` (I), `rules.apply.failed` (E) |
| `state` | `state.bookkeeping_failed` (E) |
| `store` | `store.operation_failed` (E) |
| `settings` | `settings.changed` (I) |
| `bank` | `bank.field_width_observed` (I) |
| `http` | `http.request.error` (W), `http.crossorigin.rejected` (W) |
| `enablebanking` | `.get_aspsps.completed` (I), `.start_auth.completed` (I), `.complete_auth.completed` (I) |
| `actual` | `.init.completed` (I), `.init.failed` (E), `.login.completed` (I), `.download_budget.completed` (I), `.sync.completed` (I), `.commit.completed` (I), `.commit.failed` (E), `.commit.hulc_save_failed` (W), `.schema.gap` (I), `.schema.gap_in_dependency` (W) |
| `email` | `.send.completed` (I), `.send.failed` (E) |
| `update` | `.check.completed` (I), `.check.failed` (E) |

The ones a dashboard should care about:

- **`settings.changed`** is the annotation source for every step in a matching
  series. It carries one attribute per changed setting as `"old -> new"`,
  including `auto_probability_pct`, `review_probability_pct` and
  `match_overlap_pct`.
- **`match.trial_promoted`** is the other annotation source, and the one that
  explains a `param_version` change. It carries `param_version`, `previous`,
  `settled_decisions` and `shadow_differing`.
- **`match.review_resolved`** carries `choice`, which is the same signal as
  `bankingsync_match_review_choice_total` with the payee attached — useful for
  finding the actual transactions behind a rising `other_candidate` rate.
- **The three `*_not_recorded` warnings mean evidence was lost.** The sync
  continued on purpose: a failure to write a label must never be the reason a
  budget goes un-updated. But a steady trickle of them means the model is being
  starved and nothing else will say so.
- **`transactions.dropped`** is a parse failure and means a bank changed its
  feed.

## 8. Queries that answer the real questions

### Is the matcher deciding, or asking?

```promql
sum by (outcome) (rate(bankingsync_match_probability_count[1h]))
```

Held share climbing without a threshold change means the bank's behaviour moved.
Correlate with `bankingsync_match_policy`.

### How much of the traffic sits near a threshold?

```promql
# share of decisions between the two thresholds
(
  sum(rate(bankingsync_match_probability_bucket{le="0.9"}[24h]))
  - sum(rate(bankingsync_match_probability_bucket{le="0.5"}[24h]))
) / sum(rate(bankingsync_match_probability_count[24h]))
```

Compare against `bankingsync_match_policy{setting="review_probability"}` and
`{setting="auto_probability"}` rather than hard-coding 0.5 and 0.9 — an operator
can move both.

### Is the model calibrated?

```promql
bankingsync_match_brier_score{component="score"}
bankingsync_match_brier_score{component="reliability"}   # scale error
bankingsync_match_brier_score{component="generalised_resolution"}  # discrimination
```

The decomposition that closes:

```promql
  bankingsync_match_brier_score{component="reliability"}
- bankingsync_match_brier_score{component="generalised_resolution"}
+ bankingsync_match_brier_score{component="uncertainty"}
```

should equal `{component="score"}` to floating-point precision. If it does not,
something is wrong with the exporter or the query, not with the model.

### Is the model ranking candidates correctly?

```promql
sum(rate(bankingsync_match_review_choice_total{choice="other_candidate"}[7d]))
/
sum(rate(bankingsync_match_review_choice_total{choice=~"model_best|other_candidate"}[7d]))
```

Of the reviews a person merged, the share where they picked a row other than the
one the model put first. Nothing else measures this.

### How much of the fitted model is still the shipped table?

```promql
bankingsync_match_gate{figure="levels_held_at_prior"}   # out of 17
```

```promql
count(bankingsync_match_level_observations{side="m"} == 0)
```

### Is the gate ever going to open?

```promql
bankingsync_match_gate{figure="skill_percent"}
bankingsync_match_gate{figure="p_value"}
  < on(job, instance) bankingsync_match_gate{figure="significance_level"}
bankingsync_match_gate_check{check="overall", status="promotable"}
```

Remember §3.4: these only move when somebody opens the page.

### Backfill progress and health

```promql
rate(bankingsync_transactions_added_total[5m])
histogram_quantile(0.95, sum by (le) (rate(bankingsync_budget_write_duration_seconds_bucket[30m])))
histogram_quantile(0.95, sum by (le, bank) (rate(bankingsync_fetch_duration_seconds_bucket[30m])))
```

---

## 9. Alerts worth having

Thresholds below are grounded in the program's own constants, not invented.

**A sync has not succeeded.** The default interval is 6 hours
(`SYNC_INTERVAL_HOURS`), so absence for much longer than two intervals is real.

```promql
increase(bankingsync_sync_runs_total{status="success"}[13h]) == 0
```

**The bank session is about to expire.** Re-authorisation is manual; the program
emails, but a page is better.

```promql
bankingsync_session_expiry_days < 3
```

**Transactions are being dropped.** Any value above zero is a parse failure and
means a bank changed its feed.

```promql
increase(bankingsync_transactions_dropped_total[1h]) > 0
```

**The balance has drifted.** The comparison is a deliberate design feature and a
drift is never corrected silently.

```promql
bankingsync_balance_checks_total{state="drift"} and on() changes(bankingsync_balance_drift_cents[1h]) > 0
```

**The review queue is not being worked.** Held transactions are in no budget until
somebody decides.

```promql
bankingsync_match_reviews_open > 0 and bankingsync_match_reviews_open offset 7d > 0
```

**The matcher is putting the wrong row first.** Only meaningful once there are
reviews to count — require a denominator.

```promql
(
  sum(increase(bankingsync_match_review_choice_total{choice="other_candidate"}[30d]))
  /
  sum(increase(bankingsync_match_review_choice_total{choice=~"model_best|other_candidate"}[30d]))
) > 0.2
and
sum(increase(bankingsync_match_review_choice_total[30d])) > 20
```

**The database is unhappy.** A local SQLite file answers in microseconds.

```promql
increase(bankingsync_store_operations_total{result="error"}[15m]) > 0
histogram_quantile(0.99, sum by (le) (rate(bankingsync_store_duration_seconds_bucket[15m]))) > 0.5
```

**Do not alert on** `bankingsync_match_calibration_error` below a few hundred
labels (§3.2), on `bankingsync_match_gate` staleness (§3.4), on
`bankingsync_near_miss_total` (it is informational — a near miss is the program
choosing a visible duplicate over an invisible mis-merge), or on
`bankingsync_review_problems_total{outcome="refused"}` (a refusal is the program
working).

---

## 10. Traps

1. **`param_version` churn.** Promoting parameters or moving a threshold starts a
   new `match_probability` series. Aggregate across it unless you specifically
   want to compare eras, and annotate from `settings.changed` and
   `match.trial_promoted`.
2. **Three of the model gauges are absent, not zero, before their floor.** Brier
   below 50 labels, ECE below 300, the whole gate before anybody has opened
   `/matching`. Use `absent()` deliberately.
3. **The gate gauge is as fresh as the last page load.** No series says how stale
   it is.
4. **`match_probability` excludes empty windows** and `match_margin` excludes
   unpaired transactions. Neither `_count` is the number of transactions
   processed; `bankingsync_transactions_added_total` and `_confirmed_total` are.
5. **The Brier three-way identity does not hold.** §3.2.
6. **`match_level_weight` is constant** until a promotion. That is not a broken
   exporter.
7. **`bits.*` span attributes are bits, not probabilities**, and are signed. A
   weight of −9.5 is strong evidence *against* a pairing.
8. **Database operations have no trace correlation.** §6.
9. **Every span is exported.** §6.
