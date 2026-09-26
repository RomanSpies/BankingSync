package enablebanking

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrSessionEnded reports that the session behind a request is no longer
// authorised. Nothing can be fetched for the account until it is authorised
// again, and retrying before then only repeats the refusal.
var ErrSessionEnded = errors.New("the bank session has ended; the account has to be authorised again")

// sessionEndedCodes are the error codes that mean the session is over.
//
// EXPIRED_SESSION is documented, with the instruction to "initiate a new
// authorisation and prompt end users to complete it". CLOSED_SESSION is not in
// the documentation but is what a closed session returns in production. Other
// codes that circulate in third-party integrations are left out until there is
// a source for them; an unknown code stays an ordinary error rather than being
// read as a reason to stop syncing an account.
var sessionEndedCodes = map[string]bool{
	"EXPIRED_SESSION": true,
	"CLOSED_SESSION":  true,
}

// APIError is an error response from Enable Banking.
//
// Enable Banking advises basing client logic on the error code rather than the
// HTTP status, and the two diverge in exactly the case that matters here: an
// ended session and a refused balances scope both arrive as 401.
type APIError struct {
	Status  int
	Code    string
	Message string
	Body    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("unexpected HTTP %d from Enable Banking: %s", e.Status, e.Body)
}

// Is makes errors.Is(err, ErrSessionEnded) true for the codes that end a
// session.
func (e *APIError) Is(target error) bool {
	return target == ErrSessionEnded && sessionEndedCodes[e.Code]
}

// parseAPIError reads an error response. A body that is not the documented
// shape still yields an APIError carrying the status and the raw body.
func parseAPIError(status int, body []byte) *APIError {
	e := &APIError{Status: status, Body: string(body)}
	var shape struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &shape) == nil {
		e.Code, e.Message = shape.Error, shape.Message
	}
	return e
}
