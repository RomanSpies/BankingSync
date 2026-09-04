package actual

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func budgetZip(t *testing.T, withDB bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	if withDB {
		dbPath := filepath.Join(t.TempDir(), "seed.sqlite")
		raw, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("open seed db: %v", err)
		}
		if _, err := raw.Exec(testSchema); err != nil {
			t.Fatalf("seed schema: %v", err)
		}
		_ = raw.Close()
		data, err := os.ReadFile(dbPath)
		if err != nil {
			t.Fatalf("read seed db: %v", err)
		}
		w, err := zw.Create("db.sqlite")
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}

	meta, err := zw.Create("metadata.json")
	if err != nil {
		t.Fatalf("zip meta: %v", err)
	}
	if _, err := meta.Write([]byte(`{"groupId":"group-1"}`)); err != nil {
		t.Fatalf("zip meta write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

type serverOpts struct {
	loginStatus   int
	loginBody     string
	files         string
	downloadZip   []byte
	syncStatus    int
	syncBody      []byte
	encryptKeyID  string
	syncCallCount *int
}

func newActualServer(t *testing.T, o serverOpts) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/account/login", func(w http.ResponseWriter, r *http.Request) {
		if o.loginStatus != 0 {
			w.WriteHeader(o.loginStatus)
		}
		body := o.loginBody
		if body == "" {
			body = `{"status":"ok","data":{"token":"tok-1"}}`
		}
		_, _ = w.Write([]byte(body))
	})

	mux.HandleFunc("/sync/list-user-files", func(w http.ResponseWriter, r *http.Request) {
		body := o.files
		if body == "" {
			body = fmt.Sprintf(`{"data":[{"fileId":"file-1","groupId":"group-1","name":"Budget","deleted":0,"encryptKeyId":%q}]}`, o.encryptKeyID)
		}
		_, _ = w.Write([]byte(body))
	})

	mux.HandleFunc("/sync/download-user-file", func(w http.ResponseWriter, r *http.Request) {
		z := o.downloadZip
		if z == nil {
			z = budgetZip(t, true)
		}
		_, _ = w.Write(z)
	})

	mux.HandleFunc("/sync/sync", func(w http.ResponseWriter, r *http.Request) {
		if o.syncCallCount != nil {
			*o.syncCallCount++
		}
		if o.syncStatus != 0 && o.syncStatus != http.StatusOK {
			w.WriteHeader(o.syncStatus)
			_, _ = w.Write([]byte("upstream exploded"))
			return
		}
		if o.syncBody != nil {
			_, _ = w.Write(o.syncBody)
			return
		}
		_, _ = w.Write(encodeSyncResponse(t, nil, "merkle-x"))
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func newConnectedClient(t *testing.T, o serverOpts) *Client {
	t.Helper()
	ts := newActualServer(t, o)
	c, err := NewClient(context.Background(), ts.URL, "pw", "file-1", t.TempDir())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestNewClient_happyPath(t *testing.T) {
	c := newConnectedClient(t, serverOpts{})
	if c.token != "tok-1" {
		t.Errorf("token: got %q, want tok-1", c.token)
	}
	if c.fileID != "file-1" {
		t.Errorf("fileID: got %q, want file-1", c.fileID)
	}
	if c.groupID != "group-1" {
		t.Errorf("groupID: got %q, want group-1", c.groupID)
	}
	if c.db == nil {
		t.Error("expected an opened budget database")
	}
}

func TestLogin_httpErrorIsNotReportedAsBadPassword(t *testing.T) {
	ts := newActualServer(t, serverOpts{
		loginStatus: http.StatusBadGateway,
		loginBody:   "<html><body>502 Bad Gateway</body></html>",
	})
	_, err := NewClient(context.Background(), ts.URL, "pw", "file-1", t.TempDir())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "check your password") {
		t.Errorf("a proxy 502 must not be reported as a wrong password: %v", err)
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error %q should mention the HTTP status", err.Error())
	}
}

func TestLogin_rejectedCredentialsReportReason(t *testing.T) {
	ts := newActualServer(t, serverOpts{
		loginBody: `{"status":"error","reason":"invalid-password"}`,
	})
	_, err := NewClient(context.Background(), ts.URL, "pw", "file-1", t.TempDir())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "invalid-password") {
		t.Errorf("error %q should carry the server's reason", err.Error())
	}
}

func TestSetFile_noMatchingFile(t *testing.T) {
	ts := newActualServer(t, serverOpts{files: `{"data":[]}`})
	_, err := NewClient(context.Background(), ts.URL, "pw", "missing", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "no file found") {
		t.Fatalf("expected a no-file error, got %v", err)
	}
}

func TestSetFile_ambiguousMatch(t *testing.T) {
	ts := newActualServer(t, serverOpts{
		files: `{"data":[
			{"fileId":"f1","groupId":"g1","name":"Budget","deleted":0},
			{"fileId":"f2","groupId":"g2","name":"Budget","deleted":0}
		]}`,
	})
	_, err := NewClient(context.Background(), ts.URL, "pw", "Budget", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "multiple files") {
		t.Fatalf("expected an ambiguity error, got %v", err)
	}
}

func TestSetFile_skipsDeletedFiles(t *testing.T) {
	ts := newActualServer(t, serverOpts{
		files: `{"data":[
			{"fileId":"f1","groupId":"g1","name":"Budget","deleted":1},
			{"fileId":"f2","groupId":"group-1","name":"Budget","deleted":0}
		]}`,
	})
	c, err := NewClient(context.Background(), ts.URL, "pw", "Budget", t.TempDir())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)
	if c.fileID != "f2" {
		t.Errorf("fileID: got %q, want f2 (deleted files must be ignored)", c.fileID)
	}
}

func TestNewClient_encryptedBudgetFailsEarlyAndClearly(t *testing.T) {
	ts := newActualServer(t, serverOpts{encryptKeyID: "key-123"})
	_, err := NewClient(context.Background(), ts.URL, "pw", "file-1", t.TempDir())
	if err == nil {
		t.Fatal("expected an error for an encrypted budget")
	}
	if !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("error %q should name encryption as the cause", err.Error())
	}
}

func TestDownloadBudget_archiveWithoutDatabaseIsRejected(t *testing.T) {
	ts := newActualServer(t, serverOpts{downloadZip: budgetZip(t, false)})
	_, err := NewClient(context.Background(), ts.URL, "pw", "file-1", t.TempDir())
	if err == nil {
		t.Fatal("an archive with no db.sqlite must fail")
	}
	if !strings.Contains(err.Error(), "db.sqlite") {
		t.Errorf("error %q should say the archive had no database", err.Error())
	}
}

func TestExtractZip_rejectsArchiveWithoutDatabase(t *testing.T) {
	dir := t.TempDir()
	if err := extractZip(budgetZip(t, false), dir); err == nil {
		t.Fatal("extractZip must report an archive that produced no database")
	}
	if _, err := os.Stat(filepath.Join(dir, "db.sqlite")); err == nil {
		t.Error("no db.sqlite should have been written")
	}
}

func TestExtractZip_ignoresPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("../../../../etc/evil")
	_, _ = w.Write([]byte("nope"))
	db, _ := zw.Create("nested/dir/db.sqlite")
	_, _ = db.Write([]byte("sqlite-ish"))
	_ = zw.Close()

	dir := t.TempDir()
	if err := extractZip(buf.Bytes(), dir); err != nil {
		t.Fatalf("extractZip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "db.sqlite")); err != nil {
		t.Errorf("nested db.sqlite should be flattened into the destination: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "evil") {
			t.Errorf("traversal entry escaped: %q", e.Name())
		}
	}
}

func TestCommit_restoresChangesWhenSyncFails(t *testing.T) {
	var calls int
	ts := newActualServer(t, serverOpts{syncCallCount: &calls})
	c, err := NewClient(context.Background(), ts.URL, "pw", "file-1", t.TempDir())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)

	c.db.track("transactions", "row-1", "amount", 100)
	c.db.track("transactions", "row-2", "amount", 200)

	failing := newActualServer(t, serverOpts{syncStatus: http.StatusInternalServerError})
	c.baseURL = strings.TrimRight(failing.URL, "/")

	if err := c.Commit(context.Background()); err == nil {
		t.Fatal("expected the commit to fail")
	}

	restored := c.db.FlushChanges()
	if len(restored) != 2 {
		t.Fatalf("got %d retained messages, want 2 — a failed commit must not lose local changes", len(restored))
	}
	if restored[0].Row != "row-1" || restored[1].Row != "row-2" {
		t.Errorf("restored order wrong: %s, %s", restored[0].Row, restored[1].Row)
	}
}

func TestCommit_restoresChangesForEncryptedBudget(t *testing.T) {
	c := newConnectedClient(t, serverOpts{})
	c.keyID = "key-123"
	c.db.track("transactions", "row-1", "amount", 100)

	if err := c.Commit(context.Background()); err == nil {
		t.Fatal("expected an error for an encrypted budget")
	}
	if len(c.db.FlushChanges()) != 1 {
		t.Error("changes must be retained when the commit is refused")
	}
}

func TestCommit_drainsOnSuccess(t *testing.T) {
	var calls int
	c := newConnectedClient(t, serverOpts{syncCallCount: &calls})
	c.db.track("transactions", "row-1", "amount", 100)

	if err := c.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(c.db.FlushChanges()) != 0 {
		t.Error("a successful commit must drain the buffer")
	}
	if calls == 0 {
		t.Error("expected the sync endpoint to be called")
	}
}

func TestSync_keepsOwnClientIDWhenApplyingRemoteMessages(t *testing.T) {
	remote := []*ProtoMessage{{
		Dataset: "transactions",
		Row:     "remote-1",
		Column:  "amount",
		Value:   "N:500",
	}}
	body := encodeSyncResponseWithTimestamps(t, remote,
		[]string{"2026-07-01T10:00:00.000Z-000A-FOREIGNCLIENT01"}, "merkle-x")

	c := newConnectedClient(t, serverOpts{syncBody: body})
	ownID := c.hulc.ClientID
	before := c.hulc.InitialCount

	if err := c.Resync(context.Background()); err != nil {
		t.Fatalf("Resync: %v", err)
	}

	if c.hulc.ClientID != ownID {
		t.Errorf("client ID: got %q, want our own %q — adopting a peer ID breaks HULC uniqueness", c.hulc.ClientID, ownID)
	}
	if c.hulc.InitialCount < before {
		t.Errorf("counter went backwards: %d -> %d", before, c.hulc.InitialCount)
	}
	if c.hulc.InitialCount <= 0x0A {
		t.Errorf("counter %d must exceed the remote counter 0x0A", c.hulc.InitialCount)
	}
}

func TestSync_toleratesMalformedRemoteTimestamp(t *testing.T) {
	remote := []*ProtoMessage{{Dataset: "transactions", Row: "r1", Column: "amount", Value: "N:1"}}
	body := encodeSyncResponseWithTimestamps(t, remote, []string{"not-a-hulc-timestamp"}, "m")

	c := newConnectedClient(t, serverOpts{syncBody: body})
	ownID := c.hulc.ClientID

	if err := c.Resync(context.Background()); err != nil {
		t.Fatalf("a malformed remote timestamp must not fail the sync: %v", err)
	}
	if c.hulc.ClientID != ownID {
		t.Error("client ID changed after a malformed timestamp")
	}
}

func TestSyncSync_httpErrorSurfacesBody(t *testing.T) {
	c := newConnectedClient(t, serverOpts{})
	failing := newActualServer(t, serverOpts{syncStatus: http.StatusInternalServerError})
	c.baseURL = strings.TrimRight(failing.URL, "/")

	_, err := c.syncSync(context.Background(), SyncRequest{FileID: "f", GroupID: "g"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q should carry the status", err.Error())
	}
}

func TestSnippet_truncatesLongBodies(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := snippet([]byte(long))
	if len(got) > 210 {
		t.Errorf("snippet length %d, want it truncated", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("expected an ellipsis on a truncated body")
	}
}

func TestReadGroupIDFromMeta(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"groupId":"g-42"}`), 0o644); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	if got := readGroupIDFromMeta(filepath.Join(dir, "metadata.json")); got != "g-42" {
		t.Errorf("got %q, want g-42", got)
	}
}

func TestReadGroupIDFromMeta_missingFile(t *testing.T) {
	if got := readGroupIDFromMeta(filepath.Join(t.TempDir(), "metadata.json")); got != "" {
		t.Errorf("got %q, want empty for a missing metadata.json", got)
	}
}

func encodeSyncResponse(t *testing.T, msgs []*ProtoMessage, merkle string) []byte {
	t.Helper()
	return encodeSyncResponseWithTimestamps(t, msgs, nil, merkle)
}

func encodeSyncResponseWithTimestamps(t *testing.T, msgs []*ProtoMessage, timestamps []string, merkle string) []byte {
	t.Helper()
	var envelopes []MessageEnvelope
	for i, m := range msgs {
		ts := "2026-01-01T00:00:00.000Z-0000-LOCALCLIENT00000"
		if i < len(timestamps) {
			ts = timestamps[i]
		}
		envelopes = append(envelopes, MessageEnvelope{
			Timestamp:   ts,
			IsEncrypted: false,
			Content:     m.encode(),
		})
	}
	data, err := EncodeSyncResponseForTest(envelopes, merkle)
	if err != nil {
		t.Fatalf("encode sync response: %v", err)
	}
	return data
}

var _ = json.Marshal
