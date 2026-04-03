package actual

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Client is an Actual Budget HTTP client that downloads the budget SQLite
// database, applies remote sync messages, and pushes local changes back.
type Client struct {
	httpClient *http.Client
	baseURL    string
	token      string

	fileID  string
	groupID string
	keyID   string

	dataDir string
	db      *DB
	hulc    *HULCClient
}

// NewClient authenticates against the Actual Budget server, resolves the file
// identified by syncID, downloads the budget, and performs an initial sync.
func NewClient(ctx context.Context, baseURL, password, syncID, dataDir string) (*Client, error) {
	tracer := otel.Tracer("bankingsync/actual")
	ctx, span := tracer.Start(ctx, "actual.init")
	defer span.End()

	c := &Client{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		baseURL:    strings.TrimRight(baseURL, "/"),
		dataDir:    dataDir,
	}

	if err := c.login(ctx, password); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "login failed")
		return nil, fmt.Errorf("login: %w", err)
	}
	if err := c.setFile(ctx, syncID); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "set file failed")
		return nil, fmt.Errorf("set file: %w", err)
	}
	if err := c.downloadBudget(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "download failed")
		return nil, fmt.Errorf("download budget: %w", err)
	}
	return c, nil
}

// Close releases the local SQLite database connection.
func (c *Client) Close() {
	if c.db != nil {
		c.db.Close()
	}
}

// Commit sends all pending change messages to the Actual Budget sync endpoint
// and persists the updated HULC clock.
func (c *Client) Commit(ctx context.Context) error {
	tracer := otel.Tracer("bankingsync/actual")
	ctx, span := tracer.Start(ctx, "actual.commit")
	defer span.End()

	changes := c.db.FlushChanges()
	span.SetAttributes(attribute.Int("change_count", len(changes)))
	if len(changes) == 0 && c.groupID == "" {
		return nil
	}

	req := SyncRequest{
		FileID:  c.fileID,
		GroupID: c.groupID,
		Since:   c.hulc.NullTimestamp(),
	}
	if c.keyID != "" {
		return fmt.Errorf("encrypted files are not supported by this client")
	}

	for _, msg := range changes {
		req.Messages = append(req.Messages, MessageEnvelope{
			Timestamp:   c.hulc.Timestamp(),
			IsEncrypted: false,
			Content:     msg.encode(),
		})
	}

	if c.groupID != "" {
		if _, err := c.syncSync(ctx, req); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "sync_sync failed")
			return fmt.Errorf("sync_sync: %w", err)
		}
	}

	if err := c.db.SaveHULC(c.hulc); err != nil {
		log.Printf("Warning: could not persist HULC clock: %v", err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, extraHeaders map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/"+strings.TrimLeft(path, "/"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-ACTUAL-TOKEN", c.token)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	return c.httpClient.Do(req)
}

func (c *Client) postJSON(ctx context.Context, path string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+strings.TrimLeft(path, "/"), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("X-ACTUAL-TOKEN", c.token)
	}
	return c.httpClient.Do(req)
}

func (c *Client) postProto(ctx context.Context, path string, body []byte, extraHeaders map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+strings.TrimLeft(path, "/"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-ACTUAL-TOKEN", c.token)
	req.Header.Set("Content-Type", "application/actual-sync")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	return c.httpClient.Do(req)
}

func (c *Client) login(ctx context.Context, password string) error {
	tracer := otel.Tracer("bankingsync/actual")
	ctx, span := tracer.Start(ctx, "actual.login")
	defer span.End()

	resp, err := c.postJSON(ctx, "account/login", map[string]string{
		"loginMethod": "password",
		"password":    password,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	var body struct {
		Status string `json:"status"`
		Data   struct {
			Token string `json:"token"`
		} `json:"data"`
		Reason string `json:"reason"`
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("decode login response: %w", err)
	}
	if resp.StatusCode >= 400 || body.Status == "error" {
		err := fmt.Errorf("login failed (status=%d reason=%s)", resp.StatusCode, body.Reason)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	if body.Data.Token == "" {
		return fmt.Errorf("login returned empty token — check your password")
	}
	c.token = body.Data.Token
	return nil
}

type remoteFile struct {
	FileID       string `json:"fileId"`
	GroupID      string `json:"groupId"`
	Name         string `json:"name"`
	Deleted      int    `json:"deleted"`
	EncryptKeyID string `json:"encryptKeyId"`
}

func (c *Client) listUserFiles(ctx context.Context) ([]remoteFile, error) {
	resp, err := c.get(ctx, "sync/list-user-files", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("list-user-files HTTP %d: %s", resp.StatusCode, raw)
	}
	var body struct {
		Data []remoteFile `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("decode list-user-files: %w", err)
	}
	return body.Data, nil
}

func (c *Client) setFile(ctx context.Context, id string) error {
	resp, err := c.get(ctx, "sync/list-user-files", nil)
	if err != nil {
		return err
	}
	var body struct {
		Data []remoteFile `json:"data"`
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("list-user-files HTTP %d: %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("decode list-user-files: %w", err)
	}

	var matches []remoteFile
	for _, f := range body.Data {
		if f.Deleted != 0 {
			continue
		}
		if f.FileID == id || f.Name == id || f.GroupID == id {
			matches = append(matches, f)
		}
	}
	switch len(matches) {
	case 0:
		return fmt.Errorf("no file found with id/name/group %q", id)
	case 1:
		c.fileID = matches[0].FileID
		c.groupID = matches[0].GroupID
		c.keyID = matches[0].EncryptKeyID
		return nil
	default:
		return fmt.Errorf("multiple files match %q; use the exact fileId", id)
	}
}

func (c *Client) downloadBudget(ctx context.Context) error {
	tracer := otel.Tracer("bankingsync/actual")
	ctx, span := tracer.Start(ctx, "actual.download_budget")
	defer span.End()

	if c.keyID != "" {
		return fmt.Errorf("encrypted budgets are not supported by this client (keyId=%s)", c.keyID)
	}

	dbPath := filepath.Join(c.dataDir, "db.sqlite")
	metaPath := filepath.Join(c.dataDir, "metadata.json")

	if _, err := os.Stat(dbPath); err == nil {
		if _, err := os.Stat(metaPath); err == nil {
			if cachedGroupID := readGroupIDFromMeta(metaPath); cachedGroupID == c.groupID && c.groupID != "" {
				log.Println("Re-using cached budget database")
				span.SetAttributes(attribute.Bool("cache_hit", true))
				return c.openAndSync(ctx)
			}
			log.Println("Sync ID changed on server — re-downloading budget")
			_ = os.Remove(dbPath)
			_ = os.Remove(metaPath)
		}
	}
	span.SetAttributes(attribute.Bool("cache_hit", false))

	if err := os.MkdirAll(c.dataDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", c.dataDir, err)
	}

	resp, err := c.get(ctx, "sync/download-user-file", map[string]string{
		"X-ACTUAL-FILE-ID": c.fileID,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("download-user-file: %w", err)
	}
	defer resp.Body.Close()
	zipBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("download-user-file HTTP %d", resp.StatusCode)
	}
	span.SetAttributes(attribute.Int("zip_bytes", len(zipBytes)))

	if err := extractZip(zipBytes, c.dataDir); err != nil {
		return fmt.Errorf("extract zip: %w", err)
	}

	patchMetaGroupID(metaPath, c.groupID)

	return c.openAndSync(ctx)
}

func (c *Client) openAndSync(ctx context.Context) error {
	db, err := OpenDB(filepath.Join(c.dataDir, "db.sqlite"))
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	c.db = db

	hulc, err := db.LoadHULC()
	if err != nil {
		return fmt.Errorf("load HULC: %w", err)
	}
	c.hulc = hulc

	if err := c.sync(ctx); err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}
	return nil
}

func (c *Client) sync(ctx context.Context) error {
	tracer := otel.Tracer("bankingsync/actual")
	ctx, span := tracer.Start(ctx, "actual.sync")
	defer span.End()

	req := SyncRequest{
		FileID:  c.fileID,
		GroupID: c.groupID,
		Since:   c.hulc.SinceTimestamp(),
	}

	resp, err := c.syncSync(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	msgs, err := resp.GetMessages()
	if err != nil {
		return fmt.Errorf("decode sync messages: %w", err)
	}
	span.SetAttributes(attribute.Int("message_count", len(msgs)))

	if err := c.db.ApplyMessages(msgs); err != nil {
		return fmt.Errorf("apply messages: %w", err)
	}

	if len(resp.Messages) > 0 {
		last := resp.Messages[len(resp.Messages)-1].Timestamp
		h, err := HULCFromTimestamp(last)
		if err == nil {
			c.hulc = h
		}
	}
	return nil
}

func (c *Client) syncSync(ctx context.Context, req SyncRequest) (*SyncResponse, error) {
	tracer := otel.Tracer("bankingsync/actual")
	ctx, span := tracer.Start(ctx, "actual.sync_sync",
		trace.WithAttributes(attribute.Int("request_messages", len(req.Messages))),
	)
	defer span.End()

	body := req.Encode()
	resp, err := c.postProto(ctx, "sync/sync", body, map[string]string{
		"X-ACTUAL-FILE-ID": c.fileID,
	})
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("POST sync/sync: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		err := fmt.Errorf("sync/sync HTTP %d: %s", resp.StatusCode, raw)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetAttributes(attribute.Int("response_bytes", len(raw)))
	return DecodeSyncResponse(raw)
}

func extractZip(data []byte, destDir string) error {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range r.File {

		base := filepath.Base(f.Name)
		if base != "db.sqlite" && base != "metadata.json" {
			continue
		}
		out, err := os.Create(filepath.Join(destDir, base))
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			out.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func readGroupIDFromMeta(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return ""
	}
	s, _ := meta["groupId"].(string)
	return s
}

func patchMetaGroupID(path, groupID string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return
	}
	meta["groupId"] = groupID
	patched, err := json.Marshal(meta)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, patched, 0o644)
}

// DB returns the underlying SQLite wrapper for direct access.
func (c *Client) DB() *DB { return c.db }

// GetOrCreateAccount delegates to DB.GetOrCreateAccount.
func (c *Client) GetOrCreateAccount(name string) (*Account, error) {
	return c.db.GetOrCreateAccount(name)
}

// GetTransactions delegates to DB.GetTransactions.
func (c *Client) GetTransactions(accountID string) ([]*Transaction, error) {
	return c.db.GetTransactions(accountID)
}

// CreateTransaction delegates to DB.CreateTransaction.
func (c *Client) CreateTransaction(
	date time.Time, account *Account,
	payeeName, notes string,
	amountCents int64,
	cleared bool,
	importedID string,
	importedPayee string,
) (*Transaction, error) {
	return c.db.CreateTransaction(date, account, payeeName, notes, amountCents, cleared, importedID, importedPayee)
}

// ReconcileTransaction delegates to DB.ReconcileTransaction.
func (c *Client) ReconcileTransaction(
	date time.Time, account *Account,
	payeeName, notes string,
	amountCents int64,
	cleared bool,
	importedID string,
	importedPayee string,
	alreadyMatched []*Transaction,
) (*Transaction, bool, error) {
	return c.db.ReconcileTransaction(date, account, payeeName, notes, amountCents, cleared, importedID, importedPayee, alreadyMatched)
}

// UpdateTransactionCleared delegates to DB.UpdateTransactionCleared.
func (c *Client) UpdateTransactionCleared(t *Transaction) error {
	return c.db.UpdateTransactionCleared(t)
}

// LoadRules delegates to DB.LoadRules.
func (c *Client) LoadRules() (*RuleSet, error) {
	return c.db.LoadRules()
}

// Resync fetches and applies any new messages from the server, refreshing the
// local database without committing local changes.
func (c *Client) Resync(ctx context.Context) error {
	return c.sync(ctx)
}
