package store

import (
	"fmt"
	"strconv"
	"strings"
)

// SettingPromotedTrial and SettingShadowTrial hold fitted parameter sets rather
// than operator choices, which is why they are not in Tunables and not on the
// settings form. One is in force; the other is being watched. Both are written
// by the promotion page and never by hand.
//
// Tunable settings the operator edits in the web UI. They live in the settings
// table rather than in environment variables because they are matching and
// alerting policy: the values a user needs depends on their bank's habits, and
// finding the right one is a matter of trying it.
const (
	SettingDriftNotifyCents  = "balance_drift_notify_cents"
	SettingToleranceCents    = "match_amount_tolerance_max_cents"
	SettingTolerancePercent  = "match_amount_tolerance_pct"
	SettingPayeePrefixes     = "match_payee_prefixes"
	SettingAutoProbability   = "match_auto_probability"
	SettingReviewProbability = "match_review_probability"
	SettingAskWhenUnsure     = "match_ask_when_unsure"
	SettingMatchOverlap      = "match_overlap_pct"
	SettingPromotedTrial     = "match_promoted_parameters"
	SettingShadowTrial       = "match_shadow_parameters"
	DefaultDriftNotifyCents  = 1000
	DefaultToleranceCents    = 5000
	DefaultTolerancePercent  = 25
	DefaultPayeePrefixesList = "VISA,MASTERCARD,MC,MAESTRO,DEBIT,KARTENZAHLUNG,POS"

	// The two decision thresholds, as percentages because that is the unit an
	// operator can reason about. Between them lies the band in which neither
	// merging nor importing is done automatically.
	//
	DefaultAutoProbability   = 90
	DefaultReviewProbability = 50

	// How often a transaction that reaches the matcher has a counterpart in its
	// window at all, as a percentage. It is the pi of the candidate-count prior:
	// with n candidates, a specific one starts at pi/n and "none of them" at
	// 1 - pi.
	//
	// This is a claim about an institution and not an estimate of anything, which
	// is why it is a setting. Most transactions never reach the matcher — the
	// bank's own reference settles them first — so it is not the share of
	// transactions that are settlements, but the share of the ones left over.
	//
	// A half is shipped because a half is what the program has always assumed. The
	// prior it feeds used to be written as 1/(n+1), which comes to the same
	// arithmetic at this value and to nothing else at any other, so setting it
	// here changes no behaviour and makes an assumption sayable that was not
	// sayable before.
	DefaultMatchOverlap = 50
)

// Tunables is the settings bundle the sync loop and the UI both read.
type Tunables struct {
	DriftNotifyCents int64
	ToleranceCents   int64
	TolerancePercent int
	PayeePrefixes    []string

	// AutoProbabilityPct and ReviewProbabilityPct are the two match thresholds
	// in percent. They are kept in the operator's unit here and converted where
	// the model is built, so the settings layer and the arithmetic do not have
	// to agree on a representation.
	AutoProbabilityPct   int
	ReviewProbabilityPct int

	// OverlapPct is how often a transaction reaching the matcher is expected to
	// have a counterpart at all, in percent, kept in the operator's unit for the
	// same reason the thresholds are.
	OverlapPct int

	// AskWhenUnsure turns on the one confirmation a run may ask for about a
	// decision it made alone. Off by default, and off is the honest default: it
	// is the only setting in this bundle that costs a person attention rather
	// than changing what the program does with a transaction, so it has to be
	// chosen rather than inherited.
	AskWhenUnsure bool
}

// Tunables reads the operator-tunable settings, falling back to the defaults for
// anything unset or unparseable. A malformed value must not stop a sync, so it
// is replaced rather than rejected; the UI validates on the way in.
func (s *Store) Tunables() Tunables {
	t := Tunables{
		DriftNotifyCents: DefaultDriftNotifyCents,
		ToleranceCents:   DefaultToleranceCents,
		TolerancePercent: DefaultTolerancePercent,
		PayeePrefixes:    SplitPrefixes(DefaultPayeePrefixesList),

		AutoProbabilityPct:   DefaultAutoProbability,
		ReviewProbabilityPct: DefaultReviewProbability,
		OverlapPct:           DefaultMatchOverlap,
	}

	if v, err := s.GetSetting(SettingDriftNotifyCents); err == nil && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			t.DriftNotifyCents = n
		}
	}
	if v, err := s.GetSetting(SettingToleranceCents); err == nil && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			t.ToleranceCents = n
		}
	}
	if v, err := s.GetSetting(SettingTolerancePercent); err == nil && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 100 {
			t.TolerancePercent = n
		}
	}
	if v, err := s.GetSetting(SettingPayeePrefixes); err == nil && strings.TrimSpace(v) != "" {
		t.PayeePrefixes = SplitPrefixes(v)
	}
	if v, err := s.GetSetting(SettingAutoProbability); err == nil && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			t.AutoProbabilityPct = n
		}
	}
	if v, err := s.GetSetting(SettingReviewProbability); err == nil && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			t.ReviewProbabilityPct = n
		}
	}
	if v, err := s.GetSetting(SettingMatchOverlap); err == nil && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 100 {
			t.OverlapPct = n
		}
	}
	if v, err := s.GetSetting(SettingAskWhenUnsure); err == nil {
		t.AskWhenUnsure = v == "1"
	}
	// A review threshold above the auto threshold leaves no band between them,
	// and everything that would have been asked about is merged instead. The
	// pair is therefore restored together rather than one of them being clamped
	// to the other, which would silently pick a policy nobody chose.
	if t.ReviewProbabilityPct > t.AutoProbabilityPct {
		t.AutoProbabilityPct = DefaultAutoProbability
		t.ReviewProbabilityPct = DefaultReviewProbability
	}
	return t
}

// SplitPrefixes turns the comma-separated UI value into a list.
func SplitPrefixes(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ValidateTunable checks one setting on the way in and returns the value to
// store. The UI rejects bad input rather than silently defaulting it, because a
// tolerance the user believes is set and is not is worse than an error message.
func ValidateTunable(key, value string) (string, error) {
	value = strings.TrimSpace(value)
	switch key {
	case SettingDriftNotifyCents, SettingToleranceCents:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			return "", fmt.Errorf("%s must be a whole number of cents, not %q", key, value)
		}
		return strconv.FormatInt(n, 10), nil
	case SettingTolerancePercent:
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 100 {
			return "", fmt.Errorf("%s must be a percentage between 0 and 100, not %q", key, value)
		}
		return strconv.Itoa(n), nil
	case SettingAutoProbability, SettingReviewProbability:
		// Zero is excluded rather than merely out of range: a threshold of zero
		// would adopt or hold on no evidence whatever.
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 100 {
			return "", fmt.Errorf("%s must be a percentage between 1 and 100, not %q", key, value)
		}
		return strconv.Itoa(n), nil
	case SettingMatchOverlap:
		// Both ends are excluded and neither is a rounding choice. At nought no
		// candidate could ever be a match whatever the evidence, and at a hundred
		// "none of them" would be impossible, so the model would be obliged to
		// pair every transaction with something.
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 99 {
			return "", fmt.Errorf("%s must be a percentage between 1 and 99, not %q", key, value)
		}
		return strconv.Itoa(n), nil
	case SettingAskWhenUnsure:
		return validateCheckbox(key, value)
	case SettingPayeePrefixes:
		return strings.Join(SplitPrefixes(value), ","), nil
	default:
		return "", fmt.Errorf("unknown setting %q", key)
	}
}

// validateCheckbox reads the value an HTML checkbox posts.
//
// An unticked box posts nothing at all, so an absent value is the off state
// rather than a malformed one. Rejecting it would leave the settings form with
// no way to turn such a setting back off.
func validateCheckbox(key, value string) (string, error) {
	switch value {
	case "", "0", "off":
		return "0", nil
	case "1", "on":
		return "1", nil
	default:
		return "", fmt.Errorf("%s must be on or off, not %q", key, value)
	}
}

// ValidateTunableSet checks the rules that involve more than one setting, after
// each has been validated on its own.
//
// It exists because the review threshold has no meaning except in relation to
// the automatic one: above it a match is merged unasked, below it a transaction
// is imported as new, and if the two cross there is no band left in which to ask
// anybody anything. Only the caller holding the whole form can see that, so the
// check cannot live in ValidateTunable.
func ValidateTunableSet(values map[string]string) error {
	auto, okA := values[SettingAutoProbability]
	review, okR := values[SettingReviewProbability]
	if !okA || !okR {
		return nil
	}
	a, err := strconv.Atoi(auto)
	if err != nil {
		return nil
	}
	r, err := strconv.Atoi(review)
	if err != nil {
		return nil
	}
	if r > a {
		return fmt.Errorf("the review threshold (%d%%) must not be above the automatic one (%d%%), "+
			"or nothing would ever be put up for review", r, a)
	}
	return nil
}
