package actual

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// HULCClient is a Hybrid Unique Logical Clock used to generate monotonically
// increasing, globally unique timestamps for Actual Budget sync messages.
type HULCClient struct {
	ClientID     string
	InitialCount int
	TS           time.Time
}

// NewHULCClient returns a HULCClient initialised to the Unix epoch with a
// random 16-character client ID.
func NewHULCClient() *HULCClient {
	return &HULCClient{
		ClientID:     randomClientID(),
		InitialCount: 0,
		TS:           time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// HULCFromTimestamp parses a HULC timestamp string of the form
// "2006-01-02T15:04:05.000Z-COUNTER-CLIENTID" and reconstructs the client state.
func HULCFromTimestamp(ts string) (*HULCClient, error) {

	zIdx := strings.Index(ts, "Z-")
	if zIdx < 0 {
		return nil, fmt.Errorf("invalid HULC timestamp (no 'Z-'): %q", ts)
	}
	tsStr := ts[:zIdx]
	rest := ts[zIdx+2:]

	segments := strings.Split(rest, "-")
	if len(segments) < 2 {
		return nil, fmt.Errorf("invalid HULC timestamp segments: %q", ts)
	}
	clientID := segments[len(segments)-1]
	countHex := segments[len(segments)-2]

	count, err := strconv.ParseInt(countHex, 16, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid HULC counter %q: %w", countHex, err)
	}

	t, err := time.Parse("2006-01-02T15:04:05.000", tsStr)
	if err != nil {
		return nil, fmt.Errorf("invalid HULC datetime %q: %w", tsStr, err)
	}

	return &HULCClient{
		ClientID:     clientID,
		InitialCount: int(count),
		TS:           t,
	}, nil
}

// Timestamp generates the next unique HULC timestamp, advancing the internal
// counter. An optional time may be provided; the current UTC time is used otherwise.
func (h *HULCClient) Timestamp(now ...time.Time) string {
	var t time.Time
	if len(now) > 0 && !now[0].IsZero() {
		t = now[0].UTC()
	} else {
		t = time.Now().UTC()
	}
	count := fmt.Sprintf("%04X", h.InitialCount)
	h.InitialCount++
	return fmt.Sprintf("%sZ-%s-%s", t.Format("2006-01-02T15:04:05.000"), count, h.ClientID)
}

// NullTimestamp returns a HULC timestamp anchored at the Unix epoch, used as
// the "since" value when requesting a full sync.
func (h *HULCClient) NullTimestamp() string {
	return h.Timestamp(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC))
}

// SinceTimestamp returns the HULC timestamp representing the last known server
// state, suitable for use as the "since" field in incremental sync requests.
func (h *HULCClient) SinceTimestamp() string {
	count := fmt.Sprintf("%04X", h.InitialCount)
	return fmt.Sprintf("%sZ-%s-%s", h.TS.Format("2006-01-02T15:04:05.000"), count, h.ClientID)
}

func randomClientID() string {
	id := strings.ReplaceAll(uuid.New().String(), "-", "")
	return id[len(id)-16:]
}
