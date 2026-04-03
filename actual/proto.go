package actual

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
)

func encodeValue(v any) string {
	switch x := v.(type) {
	case string:
		return "S:" + x
	case int:
		return fmt.Sprintf("N:%d", x)
	case int64:
		return fmt.Sprintf("N:%d", x)
	case float64:
		return fmt.Sprintf("N:%g", x)
	case nil:
		return "0:"
	default:
		return fmt.Sprintf("S:%v", x)
	}
}

func appendStr(b []byte, field protowire.Number, s string) []byte {
	b = protowire.AppendTag(b, field, protowire.BytesType)
	return protowire.AppendString(b, s)
}

func appendBytes(b []byte, field protowire.Number, data []byte) []byte {
	b = protowire.AppendTag(b, field, protowire.BytesType)
	return protowire.AppendBytes(b, data)
}

func appendVarint(b []byte, field protowire.Number, v uint64) []byte {
	b = protowire.AppendTag(b, field, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}

func boolUint(v bool) uint64 {
	if v {
		return 1
	}
	return 0
}

func consumeStr(b []byte) (string, []byte, error) {
	v, n := protowire.ConsumeString(b)
	if n < 0 {
		return "", nil, fmt.Errorf("protowire: %w", protowire.ParseError(n))
	}
	return v, b[n:], nil
}

func consumeBytes(b []byte) ([]byte, []byte, error) {
	v, n := protowire.ConsumeBytes(b)
	if n < 0 {
		return nil, nil, fmt.Errorf("protowire: %w", protowire.ParseError(n))
	}
	return v, b[n:], nil
}

func consumeVarint(b []byte) (uint64, []byte, error) {
	v, n := protowire.ConsumeVarint(b)
	if n < 0 {
		return 0, nil, fmt.Errorf("protowire: %w", protowire.ParseError(n))
	}
	return v, b[n:], nil
}

func skipField(b []byte, typ protowire.Type) ([]byte, error) {
	n := protowire.ConsumeFieldValue(0, typ, b)
	if n < 0 {
		return nil, fmt.Errorf("protowire skip: %w", protowire.ParseError(n))
	}
	return b[n:], nil
}

// ProtoMessage is a single cell-level change record as used by the Actual Budget sync protocol.
type ProtoMessage struct {
	Dataset string
	Row     string
	Column  string
	Value   string
}

func (m *ProtoMessage) encode() []byte {
	var b []byte
	b = appendStr(b, 1, m.Dataset)
	b = appendStr(b, 2, m.Row)
	b = appendStr(b, 3, m.Column)
	b = appendStr(b, 4, m.Value)
	return b
}

func decodeProtoMessage(raw []byte) (*ProtoMessage, error) {
	m := &ProtoMessage{}
	b := raw
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, fmt.Errorf("tag: %w", protowire.ParseError(n))
		}
		b = b[n:]
		var err error
		switch {
		case num == 1 && typ == protowire.BytesType:
			m.Dataset, b, err = consumeStr(b)
		case num == 2 && typ == protowire.BytesType:
			m.Row, b, err = consumeStr(b)
		case num == 3 && typ == protowire.BytesType:
			m.Column, b, err = consumeStr(b)
		case num == 4 && typ == protowire.BytesType:
			m.Value, b, err = consumeStr(b)
		default:
			b, err = skipField(b, typ)
		}
		if err != nil {
			return nil, err
		}
	}
	return m, nil
}

// MessageEnvelope wraps an encoded ProtoMessage with a HULC timestamp and an
// encryption flag as used in the Actual Budget sync wire format.
type MessageEnvelope struct {
	Timestamp   string
	IsEncrypted bool
	Content     []byte
}

func (e *MessageEnvelope) encode() []byte {
	var b []byte
	b = appendStr(b, 1, e.Timestamp)
	b = appendVarint(b, 2, boolUint(e.IsEncrypted))
	b = appendBytes(b, 3, e.Content)
	return b
}

func decodeMessageEnvelope(raw []byte) (*MessageEnvelope, error) {
	e := &MessageEnvelope{}
	b := raw
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, fmt.Errorf("envelope tag: %w", protowire.ParseError(n))
		}
		b = b[n:]
		var err error
		switch {
		case num == 1 && typ == protowire.BytesType:
			e.Timestamp, b, err = consumeStr(b)
		case num == 2 && typ == protowire.VarintType:
			var v uint64
			v, b, err = consumeVarint(b)
			e.IsEncrypted = v != 0
		case num == 3 && typ == protowire.BytesType:
			e.Content, b, err = consumeBytes(b)
		default:
			b, err = skipField(b, typ)
		}
		if err != nil {
			return nil, err
		}
	}
	return e, nil
}

// SyncRequest is the protobuf-encoded body sent to the Actual Budget sync endpoint.
type SyncRequest struct {
	Messages []MessageEnvelope
	FileID   string
	GroupID  string
	KeyID    string
	Since    string
}

// Encode serialises the SyncRequest to its protobuf wire representation.
func (r *SyncRequest) Encode() []byte {
	var b []byte
	for i := range r.Messages {
		b = appendBytes(b, 1, r.Messages[i].encode())
	}
	b = appendStr(b, 2, r.FileID)
	b = appendStr(b, 3, r.GroupID)
	if r.KeyID != "" {
		b = appendStr(b, 5, r.KeyID)
	}
	b = appendStr(b, 6, r.Since)
	return b
}

// SyncResponse is the decoded reply from the Actual Budget sync endpoint.
type SyncResponse struct {
	Messages []MessageEnvelope
	Merkle   string
}

// DecodeSyncResponse parses the protobuf-encoded body returned by the sync endpoint.
func DecodeSyncResponse(raw []byte) (*SyncResponse, error) {
	r := &SyncResponse{}
	b := raw
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, fmt.Errorf("response tag: %w", protowire.ParseError(n))
		}
		b = b[n:]
		var err error
		switch {
		case num == 1 && typ == protowire.BytesType:
			var envBytes []byte
			envBytes, b, err = consumeBytes(b)
			if err != nil {
				return nil, err
			}
			env, err := decodeMessageEnvelope(envBytes)
			if err != nil {
				return nil, fmt.Errorf("decode envelope: %w", err)
			}
			r.Messages = append(r.Messages, *env)
		case num == 2 && typ == protowire.BytesType:
			r.Merkle, b, err = consumeStr(b)
		default:
			b, err = skipField(b, typ)
		}
		if err != nil {
			return nil, err
		}
	}
	return r, nil
}

// GetMessages decodes all message envelopes in the response, returning an error
// if any envelope is encrypted (encryption is not supported).
func (r *SyncResponse) GetMessages() ([]*ProtoMessage, error) {
	msgs := make([]*ProtoMessage, 0, len(r.Messages))
	for _, env := range r.Messages {
		if env.IsEncrypted {
			return nil, fmt.Errorf("encrypted sync responses are not supported by this client")
		}
		m, err := decodeProtoMessage(env.Content)
		if err != nil {
			return nil, fmt.Errorf("decode message: %w", err)
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}
