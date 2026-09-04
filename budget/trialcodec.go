package budget

import (
	"encoding/json"
	"fmt"
)

// ParsePayeeLevel, ParseAmountLevel and ParseDateLevel turn a stored level name
// back into a level.
//
// By name and not by number, everywhere a level crosses a process boundary. The
// levels are an iota enumeration and inserting one in the middle would silently
// renumber every level after it, so a database holding numbers would come back
// meaning something else after an ordinary edit to the list.
func ParsePayeeLevel(s string) (PayeeLevel, bool) {
	for _, l := range []PayeeLevel{PayeeMissing, PayeeNone, PayeeConflict, PayeeSubset,
		PayeeTruncated, PayeeFuzzy, PayeeExact} {
		if l.String() == s {
			return l, true
		}
	}
	return 0, false
}

func ParseAmountLevel(s string) (AmountLevel, bool) {
	for _, l := range []AmountLevel{AmountOutsideLower, AmountOutsideHigher,
		AmountLowerWithin, AmountHigherWithin, AmountExact} {
		if l.String() == s {
			return l, true
		}
	}
	return 0, false
}

func ParseDateLevel(s string) (DateLevel, bool) {
	for _, l := range []DateLevel{DateBeforeFar, DateAfterFar, DateBeforeNear,
		DateAfterNear, DateSame} {
		if l.String() == s {
			return l, true
		}
	}
	return 0, false
}

// storedTrial is the on-disk form of a promoted parameter set.
type storedTrial struct {
	PayeeM  map[string]float64 `json:"payee_m"`
	PayeeU  map[string]float64 `json:"payee_u"`
	AmountM map[string]float64 `json:"amount_m"`
	AmountU map[string]float64 `json:"amount_u"`
	DateM   map[string]float64 `json:"date_m"`
	DateU   map[string]float64 `json:"date_u"`

	CalibrationA float64 `json:"calibration_a"`
	CalibrationB float64 `json:"calibration_b"`
}

// MarshalTrial renders a parameter set for storage.
func MarshalTrial(t Trial) ([]byte, error) {
	if t.IsZero() {
		return nil, fmt.Errorf("no parameters to store")
	}
	c := t.Calibration
	if c.A == 0 && c.B == 0 {
		c = Identity()
	}
	return json.Marshal(storedTrial{
		PayeeM:  namedPayee(t.Linkage.PayeeM),
		PayeeU:  namedPayee(t.Linkage.PayeeU),
		AmountM: namedAmount(t.Linkage.AmountM),
		AmountU: namedAmount(t.Linkage.AmountU),
		DateM:   namedDate(t.Linkage.DateM),
		DateU:   namedDate(t.Linkage.DateU),

		CalibrationA: c.A,
		CalibrationB: c.B,
	})
}

// UnmarshalTrial reads a stored parameter set back.
//
// A level the stored set does not mention is an error rather than a zero. A zero
// probability is not a missing value: it is the claim that the level cannot
// occur, which gives it an infinite weight in one direction, and quietly
// inventing that claim out of a truncated file is the worst way this could fail.
func UnmarshalTrial(raw []byte) (Trial, error) {
	var st storedTrial
	if err := json.Unmarshal(raw, &st); err != nil {
		return Trial{}, fmt.Errorf("read the stored parameters: %w", err)
	}
	var l Linkage
	var err error
	if l.PayeeM, err = payeeLevels(st.PayeeM, "payee_m"); err != nil {
		return Trial{}, err
	}
	if l.PayeeU, err = payeeLevels(st.PayeeU, "payee_u"); err != nil {
		return Trial{}, err
	}
	if l.AmountM, err = amountLevels(st.AmountM, "amount_m"); err != nil {
		return Trial{}, err
	}
	if l.AmountU, err = amountLevels(st.AmountU, "amount_u"); err != nil {
		return Trial{}, err
	}
	if l.DateM, err = dateLevels(st.DateM, "date_m"); err != nil {
		return Trial{}, err
	}
	if l.DateU, err = dateLevels(st.DateU, "date_u"); err != nil {
		return Trial{}, err
	}
	if st.CalibrationA == 0 && st.CalibrationB == 0 {
		return Trial{}, fmt.Errorf("stored calibration maps every weight to a half")
	}
	return Trial{Linkage: l, Calibration: Calibration{A: st.CalibrationA, B: st.CalibrationB}}, nil
}

func namedPayee(in map[PayeeLevel]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k.String()] = v
	}
	return out
}

func namedAmount(in map[AmountLevel]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k.String()] = v
	}
	return out
}

func namedDate(in map[DateLevel]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k.String()] = v
	}
	return out
}

func payeeLevels(in map[string]float64, field string) (map[PayeeLevel]float64, error) {
	out := make(map[PayeeLevel]float64, len(in))
	for name, v := range in {
		l, ok := ParsePayeeLevel(name)
		if !ok {
			return nil, fmt.Errorf("%s names an unknown payee level %q", field, name)
		}
		out[l] = v
	}
	if want := len(DefaultLinkage().PayeeM); len(out) != want {
		return nil, fmt.Errorf("%s has %d of %d levels", field, len(out), want)
	}
	return out, nil
}

func amountLevels(in map[string]float64, field string) (map[AmountLevel]float64, error) {
	out := make(map[AmountLevel]float64, len(in))
	for name, v := range in {
		l, ok := ParseAmountLevel(name)
		if !ok {
			return nil, fmt.Errorf("%s names an unknown amount level %q", field, name)
		}
		out[l] = v
	}
	if want := len(DefaultLinkage().AmountM); len(out) != want {
		return nil, fmt.Errorf("%s has %d of %d levels", field, len(out), want)
	}
	return out, nil
}

func dateLevels(in map[string]float64, field string) (map[DateLevel]float64, error) {
	out := make(map[DateLevel]float64, len(in))
	for name, v := range in {
		l, ok := ParseDateLevel(name)
		if !ok {
			return nil, fmt.Errorf("%s names an unknown date level %q", field, name)
		}
		out[l] = v
	}
	if want := len(DefaultLinkage().DateM); len(out) != want {
		return nil, fmt.Errorf("%s has %d of %d levels", field, len(out), want)
	}
	return out, nil
}
