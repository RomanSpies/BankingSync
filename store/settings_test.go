package store_test

import (
	"testing"

	"bankingsync/store"
)

func TestTunables_defaultsWhenUnset(t *testing.T) {
	st := openTestStore(t)
	got := st.Tunables()

	if got.TolerancePercent != store.DefaultTolerancePercent ||
		got.ToleranceCents != store.DefaultToleranceCents ||
		got.DriftNotifyCents != store.DefaultDriftNotifyCents {
		t.Errorf("defaults not applied: %+v", got)
	}
	if len(got.PayeePrefixes) == 0 {
		t.Error("the default prefix list is empty")
	}
}

// A malformed value must not stop a sync; the UI is where bad input is rejected.
func TestTunables_fallsBackOnGarbage(t *testing.T) {
	st := openTestStore(t)
	for k, v := range map[string]string{
		store.SettingTolerancePercent: "abc",
		store.SettingToleranceCents:   "-5",
		store.SettingDriftNotifyCents: "1e3",
	} {
		if err := st.SetSetting(k, v); err != nil {
			t.Fatalf("SetSetting: %v", err)
		}
	}

	got := st.Tunables()
	if got.TolerancePercent != store.DefaultTolerancePercent ||
		got.ToleranceCents != store.DefaultToleranceCents ||
		got.DriftNotifyCents != store.DefaultDriftNotifyCents {
		t.Errorf("garbage was not replaced by defaults: %+v", got)
	}
}

func TestValidateTunable_rejectsBadInput(t *testing.T) {
	bad := map[string]string{
		store.SettingTolerancePercent: "101",
		store.SettingToleranceCents:   "-1",
		store.SettingDriftNotifyCents: "abc",
	}
	for key, value := range bad {
		if _, err := store.ValidateTunable(key, value); err == nil {
			t.Errorf("%s=%q was accepted", key, value)
		}
	}

	got, err := store.ValidateTunable(store.SettingPayeePrefixes, " VISA , , MC ")
	if err != nil {
		t.Fatalf("prefixes: %v", err)
	}
	if got != "VISA,MC" {
		t.Errorf("prefixes: got %q, want VISA,MC", got)
	}

	if _, err := store.ValidateTunable("nope", "1"); err == nil {
		t.Error("an unknown setting key was accepted")
	}
}

func TestTunables_readsStoredValues(t *testing.T) {
	st := openTestStore(t)
	for k, v := range map[string]string{
		store.SettingTolerancePercent: "40",
		store.SettingToleranceCents:   "12345",
		store.SettingDriftNotifyCents: "0",
		store.SettingPayeePrefixes:    "VPAY,GIROCARD",
	} {
		if err := st.SetSetting(k, v); err != nil {
			t.Fatalf("SetSetting: %v", err)
		}
	}

	got := st.Tunables()
	if got.TolerancePercent != 40 || got.ToleranceCents != 12345 || got.DriftNotifyCents != 0 {
		t.Errorf("stored values not read back: %+v", got)
	}
	if len(got.PayeePrefixes) != 2 || got.PayeePrefixes[0] != "VPAY" {
		t.Errorf("prefixes: got %v", got.PayeePrefixes)
	}
}

func TestTunables_readsTheDecisionThresholds(t *testing.T) {
	st := openTestStore(t)

	if got := st.Tunables(); got.AutoProbabilityPct != store.DefaultAutoProbability ||
		got.ReviewProbabilityPct != store.DefaultReviewProbability {
		t.Errorf("defaults not applied: auto %d, review %d",
			got.AutoProbabilityPct, got.ReviewProbabilityPct)
	}

	for k, v := range map[string]string{
		store.SettingAutoProbability:   "97",
		store.SettingReviewProbability: "40",
	} {
		if err := st.SetSetting(k, v); err != nil {
			t.Fatalf("SetSetting: %v", err)
		}
	}
	if got := st.Tunables(); got.AutoProbabilityPct != 97 || got.ReviewProbabilityPct != 40 {
		t.Errorf("stored thresholds not read back: auto %d, review %d",
			got.AutoProbabilityPct, got.ReviewProbabilityPct)
	}
}

// TestTunables_restoresACrossedThresholdPair is the read path's own safety net.
//
// A review threshold above the automatic one leaves no band between them, so
// everything that should have been asked about is merged instead — silently,
// and in the direction that overwrites data. The UI refuses the combination on
// the way in, but the settings table can also be edited directly, and a sync
// must not carry out a policy nobody chose.
//
// Both values are restored, not just the offending one. Clamping the review
// threshold to the automatic one would leave a band of width zero, which is the
// same failure with better arithmetic.
func TestTunables_restoresACrossedThresholdPair(t *testing.T) {
	st := openTestStore(t)
	for k, v := range map[string]string{
		store.SettingAutoProbability:   "60",
		store.SettingReviewProbability: "80",
	} {
		if err := st.SetSetting(k, v); err != nil {
			t.Fatalf("SetSetting: %v", err)
		}
	}

	got := st.Tunables()
	if got.AutoProbabilityPct != store.DefaultAutoProbability ||
		got.ReviewProbabilityPct != store.DefaultReviewProbability {
		t.Errorf("a crossed pair survived: auto %d, review %d — the review band would be empty",
			got.AutoProbabilityPct, got.ReviewProbabilityPct)
	}
}

func TestValidateTunable_rejectsImpossibleThresholds(t *testing.T) {
	for _, key := range []string{store.SettingAutoProbability, store.SettingReviewProbability} {
		for _, value := range []string{"0", "101", "-5", "abc", ""} {
			if _, err := store.ValidateTunable(key, value); err == nil {
				t.Errorf("%s=%q was accepted", key, value)
			}
		}
		if _, err := store.ValidateTunable(key, "50"); err != nil {
			t.Errorf("%s=50: %v", key, err)
		}
	}
}

// TestValidateTunableSet_refusesToCloseTheReviewBand covers the rule no
// single-key check can see: the two thresholds only mean anything relative to
// each other.
func TestValidateTunableSet_refusesToCloseTheReviewBand(t *testing.T) {
	if err := store.ValidateTunableSet(map[string]string{
		store.SettingAutoProbability:   "60",
		store.SettingReviewProbability: "80",
	}); err == nil {
		t.Error("review 80% above auto 60% was accepted; nothing would ever be put up for review")
	}

	for name, values := range map[string]map[string]string{
		"review below auto": {store.SettingAutoProbability: "90", store.SettingReviewProbability: "50"},
		// Equal is allowed, and it is the narrowest legal setting rather than a
		// mistake: it leaves only genuine ties to be asked about, which is the
		// way out for an operator who wants the queue as good as switched off.
		"equal":            {store.SettingAutoProbability: "90", store.SettingReviewProbability: "90"},
		"only one present": {store.SettingAutoProbability: "90"},
	} {
		if err := store.ValidateTunableSet(values); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestValidateTunable_askWhenUnsureReadsACheckbox covers the shape this setting
// actually arrives in. An unticked checkbox posts nothing at all, so rejecting
// an empty value would make the only way to turn this off be editing the
// database.
func TestValidateTunable_askWhenUnsureReadsACheckbox(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "0"}, {"0", "0"}, {"off", "0"}, {"1", "1"}, {"on", "1"},
	} {
		got, err := store.ValidateTunable(store.SettingAskWhenUnsure, tc.in)
		if err != nil {
			t.Errorf("ValidateTunable(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ValidateTunable(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := store.ValidateTunable(store.SettingAskWhenUnsure, "yes"); err == nil {
		t.Error("an unrecognised value was accepted rather than rejected")
	}
}

// TestTunables_askingIsOffUntilItIsAskedFor is the guarantee for every
// installation that never touches this: it is the one setting that spends a
// person's attention rather than changing what happens to a transaction.
func TestTunables_askingIsOffUntilItIsAskedFor(t *testing.T) {
	st := openTestStore(t)
	if st.Tunables().AskWhenUnsure {
		t.Fatal("a fresh installation would be asked questions nobody opted into")
	}
	if err := st.SetSetting(store.SettingAskWhenUnsure, "1"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if !st.Tunables().AskWhenUnsure {
		t.Error("turning it on did not take effect")
	}
}
