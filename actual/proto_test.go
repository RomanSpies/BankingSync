package actual

import (
	"testing"
)

func TestEncodeValue(t *testing.T) {
	cases := []struct {
		input any
		want  string
	}{
		{"hello", "S:hello"},
		{"", "S:"},
		{42, "N:42"},
		{int64(-100), "N:-100"},
		{float64(3.14), "N:3.14"},
		{nil, "0:"},

		{true, "S:true"},
	}
	for _, tc := range cases {
		got := encodeValue(tc.input)
		if got != tc.want {
			t.Errorf("encodeValue(%v) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestDecodeProtoValue(t *testing.T) {
	cases := []struct {
		input string
		want  any
	}{
		{"S:hello", "hello"},
		{"S:", ""},
		{"N:42", float64(42)},
		{"N:-7", float64(-7)},
		{"N:3.14", 3.14},
		{"0:", nil},
		{"", nil},
		{"X:foo", nil},
	}
	for _, tc := range cases {
		got := decodeProtoValue(tc.input)
		if got != tc.want {
			t.Errorf("decodeProtoValue(%q) = %v (%T), want %v (%T)",
				tc.input, got, got, tc.want, tc.want)
		}
	}
}

func TestProtoMessageRoundTrip(t *testing.T) {
	cases := []ProtoMessage{
		{Dataset: "transactions", Row: "abc-123", Column: "amount", Value: "N:9999"},
		{Dataset: "payees", Row: "p1", Column: "name", Value: "S:ACME Corp"},
		{Dataset: "accounts", Row: "a1", Column: "closed", Value: "N:0"},
		{Dataset: "categories", Row: "c1", Column: "name", Value: "S:"},
	}
	for _, orig := range cases {
		encoded := orig.encode()
		decoded, err := decodeProtoMessage(encoded)
		if err != nil {
			t.Fatalf("decodeProtoMessage(%+v): %v", orig, err)
		}
		if *decoded != orig {
			t.Errorf("roundtrip mismatch:\n  got  %+v\n  want %+v", decoded, orig)
		}
	}
}

func TestDecodeProtoMessage_empty(t *testing.T) {
	m, err := decodeProtoMessage([]byte{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Dataset != "" || m.Row != "" || m.Column != "" || m.Value != "" {
		t.Errorf("expected empty ProtoMessage, got %+v", m)
	}
}

func TestMessageEnvelopeRoundTrip_unencrypted(t *testing.T) {
	inner := &ProtoMessage{Dataset: "payees", Row: "p1", Column: "name", Value: "S:ACME"}
	env := MessageEnvelope{
		Timestamp:   "2024-01-01T00:00:00.000Z-0000-AAAAAAAAAAAAAAAA",
		IsEncrypted: false,
		Content:     inner.encode(),
	}
	encoded := env.encode()
	decoded, err := decodeMessageEnvelope(encoded)
	if err != nil {
		t.Fatalf("decodeMessageEnvelope error: %v", err)
	}
	if decoded.Timestamp != env.Timestamp {
		t.Errorf("Timestamp: got %q, want %q", decoded.Timestamp, env.Timestamp)
	}
	if decoded.IsEncrypted != false {
		t.Error("IsEncrypted should be false")
	}
	got, err := decodeProtoMessage(decoded.Content)
	if err != nil {
		t.Fatalf("inner decode error: %v", err)
	}
	if *got != *inner {
		t.Errorf("inner message: got %+v, want %+v", got, inner)
	}
}

func TestMessageEnvelopeRoundTrip_encrypted(t *testing.T) {
	env := MessageEnvelope{
		Timestamp:   "2024-01-01T00:00:00.000Z-0001-BBBBBBBBBBBBBBBB",
		IsEncrypted: true,
		Content:     []byte("opaque-cipher-bytes"),
	}
	encoded := env.encode()
	decoded, err := decodeMessageEnvelope(encoded)
	if err != nil {
		t.Fatalf("decodeMessageEnvelope error: %v", err)
	}
	if !decoded.IsEncrypted {
		t.Error("IsEncrypted should be true")
	}
	if string(decoded.Content) != "opaque-cipher-bytes" {
		t.Errorf("Content: got %q, want %q", decoded.Content, "opaque-cipher-bytes")
	}
}

func TestSyncRequestEncode_roundtripViaResponse(t *testing.T) {

	inner := ProtoMessage{Dataset: "accounts", Row: "acct1", Column: "name", Value: "S:Checking"}
	env := MessageEnvelope{
		Timestamp:   "2024-01-01T00:00:00.000Z-0001-BBBBBBBBBBBBBBBB",
		IsEncrypted: false,
		Content:     inner.encode(),
	}
	req := &SyncRequest{
		Messages: []MessageEnvelope{env},
		FileID:   "file-id",
		GroupID:  "group-id",
		Since:    "1970-01-01T00:00:00.000Z-0000-BBBBBBBBBBBBBBBB",
	}
	b := req.Encode()
	if len(b) == 0 {
		t.Fatal("expected non-empty encoding")
	}
}

func TestSyncRequestEncode_noMessages(t *testing.T) {
	req := &SyncRequest{
		FileID:  "fid",
		GroupID: "gid",
		Since:   "1970-01-01T00:00:00.000Z-0000-AAAAAAAAAAAAAAAA",
	}
	b := req.Encode()
	if len(b) == 0 {
		t.Error("expected non-empty encoding even without messages")
	}
}

func TestSyncRequestEncode_withKeyID(t *testing.T) {
	req := &SyncRequest{
		FileID:  "fid",
		GroupID: "gid",
		KeyID:   "key-123",
		Since:   "1970-01-01T00:00:00.000Z-0000-AAAAAAAAAAAAAAAA",
	}
	b := req.Encode()

	if len(b) == 0 {
		t.Error("expected non-empty encoding")
	}
}

func TestDecodeSyncResponse_empty(t *testing.T) {
	resp, err := DecodeSyncResponse([]byte{})
	if err != nil {
		t.Fatalf("unexpected error on empty input: %v", err)
	}
	if len(resp.Messages) != 0 {
		t.Errorf("expected 0 messages, got %d", len(resp.Messages))
	}
	if resp.Merkle != "" {
		t.Errorf("expected empty merkle, got %q", resp.Merkle)
	}
}

func TestDecodeSyncResponse_withMessagesAndMerkle(t *testing.T) {
	inner := &ProtoMessage{Dataset: "transactions", Row: "r1", Column: "amount", Value: "N:100"}
	env := MessageEnvelope{
		Timestamp:   "2024-06-01T00:00:00.000Z-0000-CCCCCCCCCCCCCCCC",
		IsEncrypted: false,
		Content:     inner.encode(),
	}

	var raw []byte
	raw = appendBytes(raw, 1, env.encode())
	raw = appendStr(raw, 2, "merkle-hash-abc")

	resp, err := DecodeSyncResponse(raw)
	if err != nil {
		t.Fatalf("DecodeSyncResponse error: %v", err)
	}
	if len(resp.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(resp.Messages))
	}
	if resp.Merkle != "merkle-hash-abc" {
		t.Errorf("merkle: got %q, want %q", resp.Merkle, "merkle-hash-abc")
	}
}

func TestDecodeSyncResponse_multipleMessages(t *testing.T) {
	var raw []byte
	for i := 0; i < 3; i++ {
		inner := &ProtoMessage{Dataset: "payees", Row: "px", Column: "name", Value: "S:P"}
		env := MessageEnvelope{Content: inner.encode()}
		raw = appendBytes(raw, 1, env.encode())
	}

	resp, err := DecodeSyncResponse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Messages) != 3 {
		t.Errorf("expected 3 messages, got %d", len(resp.Messages))
	}
}

func TestGetMessages_success(t *testing.T) {
	inner := &ProtoMessage{Dataset: "transactions", Row: "t1", Column: "amount", Value: "N:500"}
	env := MessageEnvelope{
		Timestamp:   "ts1",
		IsEncrypted: false,
		Content:     inner.encode(),
	}
	resp := &SyncResponse{Messages: []MessageEnvelope{env}}

	msgs, err := resp.GetMessages()
	if err != nil {
		t.Fatalf("GetMessages error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Dataset != "transactions" || msgs[0].Value != "N:500" {
		t.Errorf("decoded message mismatch: %+v", msgs[0])
	}
}

func TestGetMessages_encrypted_returnsError(t *testing.T) {
	resp := &SyncResponse{
		Messages: []MessageEnvelope{
			{Timestamp: "ts", IsEncrypted: true, Content: []byte("cipher")},
		},
	}
	_, err := resp.GetMessages()
	if err == nil {
		t.Error("expected error for encrypted envelope, got nil")
	}
}

func TestGetMessages_empty(t *testing.T) {
	resp := &SyncResponse{}
	msgs, err := resp.GetMessages()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages, got %d", len(msgs))
	}
}

func TestConsumeStr_error(t *testing.T) {
	// 0xff is a continuation byte with no following bytes — truncated varint
	_, _, err := consumeStr([]byte{0xff})
	if err == nil {
		t.Error("expected error for malformed input")
	}
}

func TestConsumeBytes_error(t *testing.T) {
	_, _, err := consumeBytes([]byte{0xff})
	if err == nil {
		t.Error("expected error for malformed input")
	}
}

func TestConsumeVarint_error(t *testing.T) {
	_, _, err := consumeVarint([]byte{0xff})
	if err == nil {
		t.Error("expected error for truncated varint")
	}
}

func TestDecodeProtoMessage_unknownFieldSkipped(t *testing.T) {
	// Field 5 (unknown), varint type (0), value=42 (0x2a)
	// Tag: (5 << 3) | 0 = 40 = 0x28
	b := []byte{0x28, 0x2a}
	m, err := decodeProtoMessage(b)
	if err != nil {
		t.Fatalf("unexpected error skipping unknown field: %v", err)
	}
	if m.Dataset != "" || m.Row != "" {
		t.Errorf("expected empty message, got %+v", m)
	}
}

func TestDecodeProtoMessage_unknownFieldTruncated(t *testing.T) {
	// Field 5 (unknown), varint type, but no value bytes — forces skipField error
	b := []byte{0x28}
	_, err := decodeProtoMessage(b)
	if err == nil {
		t.Error("expected error for truncated unknown field value")
	}
}
