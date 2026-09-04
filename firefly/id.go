package firefly

import (
	"fmt"
	"strings"
)

const idSeparator = ":"

// EncodeID joins a transaction group id and the journal id of its single split
// into the opaque identifier the sync loop carries around.
//
// The identifier is later stored as "<id>|<date>" and split on the first "|", so
// neither part may contain "|" or ":". Firefly ids are decimal integers, so this
// only rejects malformed input rather than restricting legitimate values.
func EncodeID(groupID, journalID string) (string, error) {
	if err := validIDPart("group id", groupID); err != nil {
		return "", err
	}
	if err := validIDPart("journal id", journalID); err != nil {
		return "", err
	}
	return groupID + idSeparator + journalID, nil
}

// SplitID reverses EncodeID.
func SplitID(id string) (string, string, error) {
	group, journal, found := strings.Cut(id, idSeparator)
	if !found {
		return "", "", fmt.Errorf("malformed transaction id %q: want <group>:<journal>", id)
	}
	if err := validIDPart("group id", group); err != nil {
		return "", "", err
	}
	if err := validIDPart("journal id", journal); err != nil {
		return "", "", err
	}
	return group, journal, nil
}

func validIDPart(what, v string) error {
	if v == "" {
		return fmt.Errorf("empty %s", what)
	}
	if strings.ContainsAny(v, "|"+idSeparator) {
		return fmt.Errorf("%s %q contains a reserved character", what, v)
	}
	return nil
}
