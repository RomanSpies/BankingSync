package budget

import (
	"strings"
	"testing"
)

// TestTrialCodec_survivesARoundTrip is the basic requirement: parameters that go
// into the database have to come back deciding the same way.
func TestTrialCodec_survivesARoundTrip(t *testing.T) {
	pol := Policy{}
	want := ProposeTrial(DefaultLinkage(), syntheticLabels(t, 300, 21), LevelCounts{}, defaultAlpha, defaultOverlap)

	raw, err := MarshalTrial(want)
	if err != nil {
		t.Fatalf("MarshalTrial: %v", err)
	}
	got, err := UnmarshalTrial(raw)
	if err != nil {
		t.Fatalf("UnmarshalTrial: %v", err)
	}
	if got.Version(pol) != want.Version(pol) {
		t.Fatalf("the round trip changed the parameters: %s became %s",
			want.Version(pol), got.Version(pol))
	}
}

// TestTrialCodec_storesLevelsByName is what makes an ordinary edit to the level
// enumeration safe.
//
// The levels are an iota list. Storing their numbers would mean that inserting
// one in the middle silently renumbers every level after it, and a database
// written before the edit would come back describing a different model —
// without an error, and with the parameter version still agreeing.
func TestTrialCodec_storesLevelsByName(t *testing.T) {
	raw, err := MarshalTrial(Trial{Linkage: DefaultLinkage(), Calibration: Identity()})
	if err != nil {
		t.Fatalf("MarshalTrial: %v", err)
	}
	for _, want := range []string{"truncated", "outside_higher", "before_far"} {
		if !strings.Contains(string(raw), `"`+want+`"`) {
			t.Errorf("the stored form does not name the level %q: %s", want, raw)
		}
	}
}

// TestTrialCodec_refusesAnIncompleteSet keeps a truncated or hand-edited file
// from being read as a claim nobody made.
//
// A missing level would unmarshal to a probability of zero, and zero is not
// "unknown" — it is "this cannot happen", which sends the weight to infinity in
// one direction. Inventing that from a short file is the worst way this could
// fail, because it fails towards confidence.
func TestTrialCodec_refusesAnIncompleteSet(t *testing.T) {
	for name, raw := range map[string]string{
		"a missing payee level": `{"payee_m":{"exact":0.5},"payee_u":{"exact":0.5},
			"amount_m":{},"amount_u":{},"date_m":{},"date_u":{},"calibration_a":1}`,
		"an unknown level name": `{"payee_m":{"invented":0.5},"payee_u":{},
			"amount_m":{},"amount_u":{},"date_m":{},"date_u":{},"calibration_a":1}`,
		"nothing at all": `{}`,
	} {
		if _, err := UnmarshalTrial([]byte(raw)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestTrialCodec_refusesACalibrationThatMeansNothing catches the zero value,
// which is not the identity: it maps every weight to a half and would put every
// transaction in the review queue at once.
func TestTrialCodec_refusesACalibrationThatMeansNothing(t *testing.T) {
	raw, err := MarshalTrial(Trial{Linkage: DefaultLinkage(), Calibration: Identity()})
	if err != nil {
		t.Fatalf("MarshalTrial: %v", err)
	}
	broken := strings.Replace(string(raw), `"calibration_a":1`, `"calibration_a":0`, 1)
	if broken == string(raw) {
		t.Fatal("the calibration is not stored where this test expects it")
	}
	if _, err := UnmarshalTrial([]byte(broken)); err == nil {
		t.Error("a calibration that maps everything to a half was accepted")
	}
}

// TestTrialCodec_everyLevelNameParsesBack keeps the two directions in step. A
// level whose String has a name the parser does not know would be written and
// then refused on the way back in.
func TestTrialCodec_everyLevelNameParsesBack(t *testing.T) {
	for _, l := range []PayeeLevel{PayeeMissing, PayeeNone, PayeeConflict, PayeeSubset,
		PayeeTruncated, PayeeFuzzy, PayeeExact} {
		if got, ok := ParsePayeeLevel(l.String()); !ok || got != l {
			t.Errorf("payee level %q did not parse back (got %v, ok %v)", l, got, ok)
		}
	}
	for _, l := range []AmountLevel{AmountOutsideLower, AmountOutsideHigher,
		AmountLowerWithin, AmountHigherWithin, AmountExact} {
		if got, ok := ParseAmountLevel(l.String()); !ok || got != l {
			t.Errorf("amount level %q did not parse back (got %v, ok %v)", l, got, ok)
		}
	}
	for _, l := range []DateLevel{DateBeforeFar, DateAfterFar, DateBeforeNear,
		DateAfterNear, DateSame} {
		if got, ok := ParseDateLevel(l.String()); !ok || got != l {
			t.Errorf("date level %q did not parse back (got %v, ok %v)", l, got, ok)
		}
	}
}
