package enablebanking

import (
	"errors"
	"net/http"
	"testing"
)

// TestAPIError_recognisesTheCodesThatEndASession pins which codes stop an
// account from syncing. An unknown code must stay an ordinary error: reading it
// as an ended session would stop syncing an account on the strength of a guess.
func TestAPIError_recognisesTheCodesThatEndASession(t *testing.T) {
	for code, ended := range map[string]bool{
		"CLOSED_SESSION":  true,
		"EXPIRED_SESSION": true,
		"ASPSP_ERROR":     false,
		"":                false,
	} {
		err := parseAPIError(http.StatusUnauthorized, []byte(`{"code":401,"error":"`+code+`"}`))
		if got := errors.Is(err, ErrSessionEnded); got != ended {
			t.Errorf("%q: ended = %v, want %v", code, got, ended)
		}
	}
}

// TestAPIError_survivesABodyThatIsNotTheDocumentedShape keeps a proxy's HTML
// error page from turning into a nil error or a lost status.
func TestAPIError_survivesABodyThatIsNotTheDocumentedShape(t *testing.T) {
	err := parseAPIError(http.StatusBadGateway, []byte("<html>bad gateway</html>"))
	if err.Status != http.StatusBadGateway || err.Code != "" || err.Body == "" {
		t.Errorf("got %+v", err)
	}
	if errors.Is(err, ErrSessionEnded) {
		t.Error("an unparseable body was read as an ended session")
	}
}
