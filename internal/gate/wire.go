// Package gate speaks the OpenCrab core gate wire, protocol 3 (core docs/design/external-gate.md,
// "D-1099 改訂"). Frames are one JSON object plus LF over the core's Unix socket.
package gate

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxFrame is the frame limit including the LF.
const MaxFrame = 1 << 20

var ErrTooLarge = errors.New("gate: frame too large")

// ReadFrame reads one LF-terminated frame (the LF is stripped).
func ReadFrame(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > MaxFrame {
			return nil, ErrTooLarge
		}
		if err == nil {
			return buf[:len(buf)-1], nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(buf) > 0 {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
}

// Marshal encodes v like serde_json: compact, map keys sorted, no HTML escaping.
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// EncodeFrame is Marshal plus the LF, refusing frames over MaxFrame.
func EncodeFrame(v any) ([]byte, error) {
	b, err := Marshal(v)
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	if len(b) > MaxFrame {
		return nil, ErrTooLarge
	}
	return b, nil
}

type Obj = map[string]any

const (
	FinalDeliveryAutomatic       = "automatic"
	FinalDeliveryOperationDriven = "operation_driven"
)

// HelloFrame is the protocol-3 hello: the instance and the agent it speaks for, and the
// operations this gateway offers.
func HelloFrame(id, instanceID, agentID string, operations []any, finalDelivery string) Obj {
	if operations == nil {
		operations = []any{}
	}
	return Obj{
		"id":                 id,
		"m":                  "hello",
		"protocol":           3,
		"instance_id":        instanceID,
		"agent_id":           agentID,
		"operation_protocol": 1,
		"final_delivery":     finalDelivery,
		"operations":         operations,
	}
}

// Attachment is an inbound image given by absolute https URL.
type Attachment struct {
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

func ImageURL(url string) Attachment { return Attachment{Kind: "image", URL: url} }

// Caller is the role of a said's author; nil means core's default (an ordinary participant).
type Caller struct {
	Role    string `json:"role"`
	AgentID string `json:"agent_id,omitempty"`
}

var CallerOwner = &Caller{Role: "owner"}

// Said is one inbound message for a session key.
type Said struct {
	Session     string
	Origin      string
	AuthorID    string
	AuthorLabel string
	Text        string
	Attachments []Attachment
	Caller      *Caller
	// Ref is the key other messages use to point at this one; ReplyTarget is the ref this one
	// answers.
	Ref         string
	ReplyTarget string
}

func SaidFrame(id string, s Said) Obj {
	atts := s.Attachments
	if atts == nil {
		atts = []Attachment{}
	}
	f := Obj{
		"id":          id,
		"m":           "said",
		"session":     s.Session,
		"origin":      s.Origin,
		"author_id":   s.AuthorID,
		"text":        s.Text,
		"attachments": atts,
	}
	if s.AuthorLabel != "" {
		f["author_label"] = s.AuthorLabel
	}
	if s.Caller != nil {
		f["caller"] = s.Caller
	}
	if s.Ref != "" {
		f["ref"] = s.Ref
	}
	if s.ReplyTarget != "" {
		f["reply_target"] = s.ReplyTarget
	}
	return f
}

func OpenFrame(id, session string) Obj { return Obj{"id": id, "m": "open", "session": session} }

func OkFrame(id string) Obj { return Obj{"id": id, "m": "ok"} }

// InvokeOkFrame answers an invoke; result may be nil (JSON null).
func InvokeOkFrame(id string, result any) Obj { return Obj{"id": id, "m": "ok", "result": result} }

func ErrFrame(id, code string) Obj { return Obj{"id": id, "m": "err", "code": code, "detail": nil} }

// Msg is a frame from core, decoded loosely: only the fields this gateway reads.
type Msg struct {
	M       string          `json:"m"`
	ID      *string         `json:"id"`
	Session string          `json:"session"`
	Payload json.RawMessage `json:"payload"`
	// invoke
	Operation          string `json:"operation"`
	DeclarationDigest  string `json:"declaration_digest"`
	Dispatch           string `json:"dispatch"`
	Effect             string `json:"effect"`
	InvocationProtocol int    `json:"invocation_protocol"`
	// ok / err
	Seq       json.RawMessage `json:"seq"`
	SessionID string          `json:"session_id"`
	Code      string          `json:"code"`
	Detail    *string         `json:"detail"`
}

func (m *Msg) IDString() string {
	if m.ID == nil {
		return ""
	}
	return *m.ID
}

// ParseMsg decodes one frame. A frame that is not a JSON object with a string `m` is an error.
func ParseMsg(frame []byte) (*Msg, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(frame, &raw); err != nil {
		return nil, fmt.Errorf("gate: bad frame: %w", err)
	}
	var m Msg
	if err := json.Unmarshal(frame, &m); err != nil {
		return nil, fmt.Errorf("gate: bad frame: %w", err)
	}
	if m.M == "" {
		return nil, errors.New("gate: frame without m")
	}
	return &m, nil
}

// SayText is the say payload's text; empty when missing, not a string, or empty.
func SayText(payload json.RawMessage) (text, replyTarget string) {
	var p struct {
		Text        any `json:"text"`
		ReplyTarget any `json:"reply_target"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return "", ""
	}
	text, _ = p.Text.(string)
	replyTarget, _ = p.ReplyTarget.(string)
	return text, replyTarget
}

// DeclarationDigest is core's runtime declaration digest of the hello's operations
// (extgate operations.rs runtime_declaration_digest): sha256 of the canonical snapshot.
func DeclarationDigest(operations []any, finalDelivery string) (string, error) {
	canon := make([]any, 0, len(operations))
	for _, op := range operations {
		o, ok := op.(Obj)
		if !ok {
			return "", errors.New("gate: operation is not an object")
		}
		auth, _ := o["authorization"].(Obj)
		c := Obj{"authorization": Obj{"allowed_callers": auth["allowed_callers"]}}
		for _, k := range []string{"name", "description", "input_schema", "output_schema", "callback_schema", "dispatch", "sub_engine", "sharing", "effect"} {
			c[k] = o[k]
		}
		canon = append(canon, c)
	}
	b, err := Marshal(Obj{"version": 1, "final_delivery": finalDelivery, "operations": canon})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
