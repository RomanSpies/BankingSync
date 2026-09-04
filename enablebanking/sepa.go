package enablebanking

import (
	"regexp"
	"strings"
)

var sepaTagPattern = regexp.MustCompile(`\b(EREF|KREF|MREF|CRED|DEBT|COAM|OAMT|SVWZ|ABWA|ABWE|IBAN|BIC)\+`)

var sepaPurposeTags = map[string]bool{
	"SVWZ": true,
	"ABWA": true,
	"ABWE": true,
}

type SEPARefs struct {
	EndToEnd   string
	Mandate    string
	CreditorID string
}

func (r SEPARefs) isZero() bool {
	return r.EndToEnd == "" && r.Mandate == "" && r.CreditorID == ""
}

func (r *SEPARefs) merge(o SEPARefs) {
	if r.EndToEnd == "" {
		r.EndToEnd = o.EndToEnd
	}
	if r.Mandate == "" {
		r.Mandate = o.Mandate
	}
	if r.CreditorID == "" {
		r.CreditorID = o.CreditorID
	}
}

func stripSEPAPrefixes(s string) string {
	purpose, _ := parseSEPA(s)
	return purpose
}

func parseSEPA(s string) (string, SEPARefs) {
	loc := sepaTagPattern.FindAllStringSubmatchIndex(s, -1)
	if len(loc) == 0 {
		return s, SEPARefs{}
	}

	type segment struct {
		tag   string
		value string
	}
	var segments []segment
	for i, m := range loc {
		tag := s[m[2]:m[3]]
		valueStart := m[1]
		valueEnd := len(s)
		if i+1 < len(loc) {
			valueEnd = loc[i+1][0]
		}
		segments = append(segments, segment{
			tag:   tag,
			value: strings.TrimSpace(s[valueStart:valueEnd]),
		})
	}

	var refs SEPARefs
	for _, seg := range segments {
		switch seg.tag {
		case "EREF":
			if refs.EndToEnd == "" {
				refs.EndToEnd = seg.value
			}
		case "MREF":
			if refs.Mandate == "" {
				refs.Mandate = seg.value
			}
		case "CRED":
			if refs.CreditorID == "" {
				refs.CreditorID = seg.value
			}
		}
	}

	for _, seg := range segments {
		if seg.tag == "SVWZ" && seg.value != "" {
			return seg.value, refs
		}
	}

	var kept []string
	if lead := strings.TrimSpace(s[:loc[0][0]]); lead != "" {
		kept = append(kept, lead)
	}
	for _, seg := range segments {
		if sepaPurposeTags[seg.tag] && seg.value != "" {
			kept = append(kept, seg.value)
		}
	}
	if len(kept) == 0 {
		return "", refs
	}
	return strings.Join(kept, " "), refs
}
