package fireflylive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// NamePrefix marks every account these tests create. AssertDisposable refuses to
// write to an instance holding an asset account without it, so the prefix is a
// safety device rather than a naming convention.
const NamePrefix = "bstest-"

// Env is the instance to test against, as named by the environment.
type Env struct {
	BaseURL string
	RunID   string
}

// ErrNoInstance reports that no live instance was named. Callers turn this into
// a hard failure rather than a skip: the build tag already decided whether these
// tests run, and once compiled in they must either run or fail. A skip here
// would produce the one outcome this arrangement exists to prevent — a green
// pipeline that tested nothing.
var ErrNoInstance = errors.New("no live Firefly instance configured")

// EnvFromOS reads the three variables the live tests need and explains which one
// is missing. Nothing is defaulted: an instance nobody named on purpose is an
// instance nobody meant to have written to.
func EnvFromOS() (Env, error) {
	e := Env{
		BaseURL: strings.TrimRight(os.Getenv("FIREFLY_LIVE_URL"), "/"),
		RunID:   os.Getenv("FIREFLY_LIVE_RUN_ID"),
	}
	switch {
	case e.BaseURL == "":
		return e, fmt.Errorf("%w: FIREFLY_LIVE_URL is not set, so the fireflylive "+
			"build tag was used without naming an instance", ErrNoInstance)
	case e.RunID == "":
		return e, fmt.Errorf("%w: FIREFLY_LIVE_RUN_ID is not set; without it two runs "+
			"against the same instance would share account names", ErrNoInstance)
	case !strings.EqualFold(os.Getenv("FIREFLY_LIVE_DESTRUCTIVE"), "yes"):
		return e, fmt.Errorf("%w: FIREFLY_LIVE_DESTRUCTIVE=yes is required, because "+
			"these tests create and modify accounts and transactions in whatever "+
			"instance they are pointed at", ErrNoInstance)
	}
	return e, nil
}

// Namespace keys account names to one run and one test.
//
// Derived from the test name rather than a counter, because the same test has to
// resolve the same account across several calls, and because someone staring at
// a red pipeline wants to find that account in Firefly's UI.
func Namespace(runID, testName string) string {
	sum := sha256.Sum256([]byte(runID + "/" + testName))
	return NamePrefix + hex.EncodeToString(sum[:])[:10] + "-"
}

// AssertDisposable refuses to write to an instance holding anything this suite
// did not create.
//
// One request, and it is the difference between a red pipeline and forty test
// transactions in somebody's real budget. Asset accounts only: expense and
// revenue accounts appear on their own as a side effect of importing, so
// demanding a prefix from them would reject an instance this suite had used
// correctly.
func AssertDisposable(ctx context.Context, baseURL, token string) error {
	for page := 1; ; page++ {
		body, err := apiGet(ctx, baseURL, token,
			fmt.Sprintf("/api/v1/accounts?type=asset&limit=100&page=%d", page))
		if err != nil {
			return fmt.Errorf("listing accounts to check the instance is disposable: %w", err)
		}
		var out struct {
			Data []struct {
				Attributes struct {
					Name string `json:"name"`
				} `json:"attributes"`
			} `json:"data"`
			Meta struct {
				Pagination struct {
					TotalPages int `json:"total_pages"`
				} `json:"pagination"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return fmt.Errorf("decoding the account list: %w", err)
		}
		for _, a := range out.Data {
			if !strings.HasPrefix(a.Attributes.Name, NamePrefix) {
				return fmt.Errorf("the instance holds an asset account %q that this suite did "+
					"not create; point FIREFLY_LIVE_URL at a disposable instance", a.Attributes.Name)
			}
		}
		// Every page is walked rather than only the first. A check that stops at
		// one page would wave through an instance whose real accounts happen to
		// sort after the test's own.
		if len(out.Data) == 0 || page >= out.Meta.Pagination.TotalPages {
			return nil
		}
	}
}

// DeleteTransactionGroup removes a transaction group.
//
// It lives here rather than on firefly.Client on purpose. Production never
// deletes anything — a group that has gone missing becomes budget.ErrGone — and
// giving the client a destructive verb it never calls, so that a test can use it,
// would be the wrong answer to a test's need.
func DeleteTransactionGroup(ctx context.Context, baseURL, token, groupID string) error {
	req, err := authed(ctx, http.MethodDelete, baseURL, "/api/v1/transactions/"+groupID, token, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("deleting transaction group %s: %w", groupID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deleting transaction group %s: HTTP %d", groupID, resp.StatusCode)
	}
	return nil
}

func apiGet(ctx context.Context, baseURL, token, path string) ([]byte, error) {
	req, err := authed(ctx, http.MethodGet, baseURL, path, token, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, excerpt(buf.String()))
	}
	return buf.Bytes(), nil
}

func authed(ctx context.Context, method, baseURL, path, token string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}
