package enablebanking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Balance is one entry from GET /accounts/{uid}/balances.
type Balance struct {
	Name          string
	Type          string
	AmountCents   int64
	Currency      string
	ReferenceDate time.Time
	LastChange    string
	LastCommitted string
}

// HasReferenceDate reports whether the ASPSP dated this balance. Without a date
// the caller cannot tell which transactions the figure already contains.
func (b Balance) HasReferenceDate() bool { return !b.ReferenceDate.IsZero() }

var (
	// ErrBalancesNotPermitted means the session carries no balances consent.
	// Enable Banking fixes that scope at authorisation time, so the only remedy
	// is re-authorising the bank.
	ErrBalancesNotPermitted = errors.New("balances access was not granted for this session")

	// ErrBalancesUnsupported means the ASPSP exposes no balances endpoint.
	ErrBalancesUnsupported = errors.New("this bank does not expose balances")

	// ErrNoBookedBalance means the bank answered, but with no balance type this
	// program is willing to compute an opening balance from.
	ErrNoBookedBalance = errors.New("no booked balance type available")

	// ErrAmbiguousBalance means several balances of the winning type survived the
	// currency filter. Picking one would be a guess.
	ErrAmbiguousBalance = errors.New("more than one balance of the selected type")
)

// bookedPreference lists the balance types that report posted entries and
// nothing else, best first. These are always preferred.
var bookedPreference = []string{"CLBD", "ITBD", "PRCD"}

// availablePreference is the fallback for banks that report no booked type at
// all. Revolut is one: it answers with ITAV only, and refusing it would leave
// those accounts without an opening balance forever.
//
// An available balance is *disposable funds*, not "booked plus pending", so
// using one carries a caveat that cannot be resolved from the response:
//
//   - It already reflects card holds, which is why IncludesPending exists: the
//     transaction sum has to cover pending entries too, or they are subtracted
//     from a figure that never contained them.
//   - At a bank that grants an overdraft it also contains the credit line, and
//     the line is not in the response. The resulting opening balance is then too
//     high by exactly that amount. The selected type is therefore recorded and
//     shown to the user, so the figure can be checked against the banking app
//     rather than taken on faith.
//
// Types excluded from both lists stay excluded: OPBD/OPAV are as of the start of
// a period, FWAV/XPCD are forward-looking and include entries that have not
// happened, and VALU/INFO/OTHR have no semantics fixed by the standard.
var availablePreference = []string{"ITAV", "CLAV"}

// IncludesPending reports whether this balance already accounts for authorised
// but unbooked entries. It decides whether the opening-balance arithmetic has to
// subtract pending transactions as well as booked ones.
func (b Balance) IncludesPending() bool {
	for _, t := range availablePreference {
		if b.Type == t {
			return true
		}
	}
	return false
}

// FetchBalances retrieves the balances of one account.
func (c *Client) FetchBalances(ctx context.Context, accountUID string) ([]Balance, error) {
	tracer := otel.Tracer("bankingsync/enablebanking")
	ctx, span := tracer.Start(ctx, "enablebanking.fetch_balances")
	defer span.End()

	headers, err := c.makeHeaders()
	if err != nil {
		return nil, fmt.Errorf("makeHeaders: %w", err)
	}

	url := fmt.Sprintf("%s/accounts/%s/balances", c.baseURL, accountUID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	body, status, err := c.doWithRateLimitRetry(ctx, req, headers)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w (HTTP %d): %s", ErrBalancesNotPermitted, status, body)
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w (HTTP %d)", ErrBalancesUnsupported, status)
	default:
		return nil, fmt.Errorf("unexpected HTTP %d from Enable Banking balances: %s", status, body)
	}

	captureResponse("balances", "balances", body)

	var data struct {
		Balances []map[string]any `json:"balances"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("decode balances: %w", err)
	}

	out := make([]Balance, 0, len(data.Balances))
	for _, raw := range data.Balances {
		b, err := parseBalance(raw)
		if err != nil {
			log.Printf("Skipping malformed balance: %v | %v", err, raw)
			continue
		}
		out = append(out, b)
	}
	span.SetAttributes(attribute.Int("balance_count", len(out)))
	return out, nil
}

func parseBalance(raw map[string]any) (Balance, error) {
	b := Balance{
		Name:          jsonString(raw, "name"),
		Type:          strings.ToUpper(jsonString(raw, "balance_type")),
		LastChange:    jsonString(raw, "last_change_date_time"),
		LastCommitted: jsonString(raw, "last_committed_transaction"),
	}
	if b.Type == "" {
		return Balance{}, errors.New("balance_type missing")
	}

	amt, ok := raw["balance_amount"].(map[string]any)
	if !ok {
		return Balance{}, errors.New("balance_amount missing")
	}
	cents, err := ParseDecimalCents(jsonString(amt, "amount"))
	if err != nil {
		return Balance{}, fmt.Errorf("balance_amount.amount: %w", err)
	}
	b.AmountCents = cents
	b.Currency = strings.ToUpper(jsonString(amt, "currency"))

	if rd := jsonString(raw, "reference_date"); rd != "" {
		d, err := time.Parse("2006-01-02", rd)
		if err != nil {
			return Balance{}, fmt.Errorf("reference_date %q: %w", rd, err)
		}
		b.ReferenceDate = d
	}
	return b, nil
}

// SelectBalance picks the balance an opening balance may be computed from: a
// booked type when the bank offers one, an available type only when it does not.
//
// currency is the account's own currency; when it is empty the balances must
// agree on one currency among themselves, because netting two currencies would
// be meaningless.
func SelectBalance(balances []Balance, currency string) (Balance, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))

	candidates := make([]Balance, 0, len(balances))
	for _, b := range balances {
		if currency != "" && b.Currency != "" && b.Currency != currency {
			continue
		}
		candidates = append(candidates, b)
	}

	if currency == "" {
		seen := ""
		for _, b := range candidates {
			if b.Currency == "" {
				continue
			}
			if seen != "" && b.Currency != seen {
				return Balance{}, fmt.Errorf(
					"%w: account currency is unknown and the bank reported both %s and %s",
					ErrAmbiguousBalance, seen, b.Currency)
			}
			seen = b.Currency
		}
	}

	for _, want := range append(append([]string{}, bookedPreference...), availablePreference...) {
		var hits []Balance
		for _, b := range candidates {
			if b.Type != want {
				continue
			}
			// PRCD is the previous period's closing balance. Without a date there
			// is no way to know where to cut the transaction sum, so it is not a
			// usable figure rather than a slightly stale one.
			if want == "PRCD" && !b.HasReferenceDate() {
				continue
			}
			hits = append(hits, b)
		}
		switch len(hits) {
		case 0:
			continue
		case 1:
			return hits[0], nil
		default:
			return Balance{}, fmt.Errorf(
				"%w: %d balances of type %s in %s", ErrAmbiguousBalance, len(hits), want, currency)
		}
	}

	return Balance{}, fmt.Errorf("%w (bank reported: %s)", ErrNoBookedBalance, describeTypes(balances))
}

func describeTypes(balances []Balance) string {
	if len(balances) == 0 {
		return "none"
	}
	seen := make(map[string]struct{}, len(balances))
	out := make([]string, 0, len(balances))
	for _, b := range balances {
		if _, dup := seen[b.Type]; dup {
			continue
		}
		seen[b.Type] = struct{}{}
		out = append(out, b.Type)
	}
	return strings.Join(out, ", ")
}

// ParseDecimalCents converts a decimal amount string to int64 cents without
// touching a float.
//
// It refuses any value it cannot represent exactly rather than truncating: a
// three-decimal currency such as BHD, or a sub-cent balance, would otherwise
// lose money silently at the one point in this program where the number is
// written once and never reconciled again.
func ParseDecimalCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty amount")
	}

	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg = true
		s = s[1:]
	}
	if s == "" {
		return 0, errors.New("amount is only a sign")
	}

	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if strings.Contains(fracPart, ".") {
		return 0, fmt.Errorf("more than one decimal point in %q", s)
	}
	if intPart == "" {
		return 0, fmt.Errorf("no digits before the decimal point in %q", s)
	}
	if hasDot && fracPart == "" {
		return 0, fmt.Errorf("trailing decimal point in %q", s)
	}
	if !allDigits(intPart) || (fracPart != "" && !allDigits(fracPart)) {
		return 0, fmt.Errorf("not a plain decimal number: %q", s)
	}

	if len(fracPart) > 2 {
		for _, r := range fracPart[2:] {
			if r != '0' {
				return 0, fmt.Errorf("%q has sub-cent precision that cannot be represented exactly", s)
			}
		}
		fracPart = fracPart[:2]
	}
	for len(fracPart) < 2 {
		fracPart += "0"
	}

	v, err := strconv.ParseInt(intPart+fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q out of range: %w", s, err)
	}
	if neg {
		v = -v
	}
	return v, nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// BalanceAccessEnv opts out of requesting the balances consent scope, for a bank
// that rejects the authorisation request when it is asked for.
const BalanceAccessEnv = "EB_REQUEST_BALANCE_ACCESS"

// RequestBalanceAccess reports whether authorisation should ask for balances.
func RequestBalanceAccess() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv(BalanceAccessEnv)), "false")
}
