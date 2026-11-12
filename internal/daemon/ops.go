package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/kojira/opencrab-bluesky-gateway/internal/bsky"
	"github.com/kojira/opencrab-bluesky-gateway/internal/gate"
	"github.com/kojira/opencrab-bluesky-gateway/internal/inbound"
)

const endTurn = "結果は返らない。この呼び出しだけではターンは終わらない。これで終えるなら、同じ応答の最後の行に NO_REPLY だけを書く。This call returns nothing. It does not end the turn by itself; to end the turn, write NO_REPLY alone on the final line of this same response."

// Operations is the hello declaration, sorted by name as core requires. withFiles adds the
// `files` field (workspace images) to post and reply.
func Operations(withFiles bool) []any {
	declare := func(name, desc string, input gate.Obj, dispatch, effect, sharing string) gate.Obj {
		return gate.Obj{
			"name":            name,
			"description":     desc,
			"input_schema":    input,
			"output_schema":   nil,
			"callback_schema": nil,
			"authorization":   gate.Obj{"allowed_callers": []any{"co_agent", "guest", "owner", "trusted"}},
			"dispatch":        dispatch,
			"sub_engine":      "allowed", // a subtask is the agent itself (kojira 2026-11-06)
			"sharing":         sharing,
			"effect":          effect,
		}
	}
	decl := func(name, desc string, input gate.Obj, utterance bool, sharing string) gate.Obj {
		if utterance {
			return declare(name, desc, input, "utterance", "utterance", sharing)
		}
		return declare(name, desc, input, "background", "state_change", sharing)
	}
	str := func(desc string) gate.Obj { return gate.Obj{"type": "string", "description": desc} }
	ref := func(desc string) gate.Obj {
		return gate.Obj{"type": "string", "description": desc, "format": "short-ref"}
	}
	files := gate.Obj{
		"type":        "array",
		"items":       gate.Obj{"type": "string", "format": "workspace-file"},
		"description": "Workspace image files (JPEG, PNG, WebP, GIF), up to 4, each under 1 MB.",
	}
	post := gate.Obj{"type": "object", "required": []any{"text"}, "properties": gate.Obj{"text": str("投稿本文（300文字を超えるとスレッドに分けて投稿する）")}}
	reply := gate.Obj{"type": "object", "required": []any{"event", "text"}, "properties": gate.Obj{
		"event": ref("返信先の投稿（e番号か at:// の uri）"),
		"text":  str("返信本文"),
	}}
	if withFiles {
		post["properties"].(gate.Obj)["files"] = files
		reply["properties"].(gate.Obj)["files"] = files
	}
	ops := []gate.Obj{
		decl("follow", "相手をフォローする。user に会話の u番号、または handle（例 alice.bsky.social）か did を渡す。",
			gate.Obj{"type": "object", "required": []any{"user"}, "properties": gate.Obj{"user": ref("フォローする相手（u番号、handle、did）")}},
			false, "agent_bound"),
		decl("like", "投稿にいいねする。event に会話の e番号か at:// の uri（read_* の結果に出る）。"+endTurn,
			gate.Obj{"type": "object", "required": []any{"event"}, "properties": gate.Obj{"event": ref("対象の投稿（e番号か at:// の uri）")}},
			true, "conversation_bound"),
		decl("send_dm", "このDMの相手にメッセージを送る（DMの会話の中だけで使える）。text に本文。地の文で書いても同じくこのDMに送られる。"+endTurn,
			gate.Obj{"type": "object", "required": []any{"text"}, "properties": gate.Obj{"text": str("送る本文（1000文字を超えると分けて送る）")}},
			true, "conversation_bound"),
		decl("post", "誰への返信でもない、新しい投稿をする（相手のスレッドには出ない）。text に本文。ツールを使わずに本文を書いても同じく新規投稿になる。"+endTurn, post, true, "conversation_bound"),
		decl("reply", "reply を呼ぶ応答では、地の文に何も書かない（最後の行の NO_REPLY だけは書いてよい）。地の文は別の投稿になるので、返信が二重になる。投稿に返信する。event に会話の e番号か at:// の uri（read_* の結果に出る）、text に返信本文。"+endTurn, reply, true, "conversation_bound"),
		decl("repost", "投稿をリポストする。event に会話の e番号か at:// の uri（read_* の結果に出る）。"+endTurn,
			gate.Obj{"type": "object", "required": []any{"event"}, "properties": gate.Obj{"event": ref("対象の投稿（e番号か at:// の uri）")}},
			true, "conversation_bound"),
	}
	ops = append(ops, readOperations(declare)...)
	sort.Slice(ops, func(i, j int) bool { return ops[i]["name"].(string) < ops[j]["name"].(string) })
	out := make([]any, len(ops))
	for i, o := range ops {
		out[i] = o
	}
	return out
}

// Poster is the part of the Bluesky client the operations use (a stub in tests).
type Poster interface {
	Post(ctx context.Context, text, replyTo string, images []bsky.Image) ([]string, error)
	SendDM(ctx context.Context, convoID, text string) ([]string, error)
	Like(ctx context.Context, uri string) (string, error)
	Repost(ctx context.Context, uri string) (string, error)
	Follow(ctx context.Context, who string) (string, error)
	Reader
}

// Ops runs core's invokes and says for one bot. Bsky is nil while the bot has no account.
type Ops struct {
	Bsky       Poster
	OutboxRoot string
	// AgentID reads a session key back to its DM conversation (inbound.DMConvo).
	AgentID string
}

// sendText is where plain text of a session goes: its DM conversation for a DM session, else a
// post (a reply when replyTarget is set). The one routing rule for say and send_dm.
func (o *Ops) sendText(ctx context.Context, session, text, replyTarget string, images []bsky.Image) ([]string, error) {
	if convo := inbound.DMConvo(session, o.AgentID); convo != "" {
		return o.Bsky.SendDM(ctx, convo, text)
	}
	return o.Bsky.Post(ctx, text, replyTarget, images)
}

var errNoAccount = errors.New("the bot has no Bluesky account yet")

// isPostURI reports whether an event is a post uri: core resolves e-numbers to uris, and the
// read operations show uris the model can pass as they are. One check for every op taking event.
func isPostURI(event string) bool { return strings.HasPrefix(event, "at://") }

func (o *Ops) Say(ctx context.Context, session, text, replyTarget string) error {
	if o.Bsky == nil {
		return errNoAccount
	}
	_, err := o.sendText(ctx, session, text, replyTarget, nil)
	return err
}

func outcomeOf(err error) gate.Outcome {
	var apiErr *bsky.APIError
	if errors.As(err, &apiErr) && apiErr.Permanent() {
		return gate.OutcomeRejected
	}
	return gate.OutcomeIndeterminate
}

func (o *Ops) Invoke(ctx context.Context, session string, operation string, raw json.RawMessage) (any, gate.Outcome) {
	var p struct {
		Text  string            `json:"text"`
		Event string            `json:"event"`
		User  string            `json:"user"`
		Files []json.RawMessage `json:"files"`
	}
	if o.Bsky == nil || json.Unmarshal(raw, &p) != nil {
		return nil, gate.OutcomeRejected
	}
	if result, outcome, ok := o.read(ctx, operation, raw); ok {
		return result, outcome
	}
	switch operation {
	case "send_dm":
		convo := inbound.DMConvo(session, o.AgentID)
		if convo == "" || strings.TrimSpace(p.Text) == "" {
			return nil, gate.OutcomeRejected
		}
		ids, err := o.sendText(ctx, session, p.Text, "", nil)
		if err != nil {
			if len(ids) > 0 {
				return nil, gate.OutcomeIndeterminate
			}
			return nil, outcomeOf(err)
		}
		return nil, gate.OutcomeOk
	case "post", "reply":
		if operation == "reply" && !isPostURI(p.Event) {
			return nil, gate.OutcomeRejected
		}
		images, err := o.images(p.Files)
		if err != nil || (strings.TrimSpace(p.Text) == "" && len(images) == 0) {
			return nil, gate.OutcomeRejected
		}
		target := ""
		if operation == "reply" {
			target = p.Event
		}
		uris, err := o.Bsky.Post(ctx, p.Text, target, images)
		if err != nil {
			if len(uris) > 0 {
				return nil, gate.OutcomeIndeterminate
			}
			return nil, outcomeOf(err)
		}
		return nil, gate.OutcomeOk
	case "like", "repost":
		if !isPostURI(p.Event) {
			return nil, gate.OutcomeRejected
		}
		var err error
		if operation == "like" {
			_, err = o.Bsky.Like(ctx, p.Event)
		} else {
			_, err = o.Bsky.Repost(ctx, p.Event)
		}
		if err != nil {
			return nil, outcomeOf(err)
		}
		return nil, gate.OutcomeOk
	case "follow":
		if strings.TrimSpace(p.User) == "" {
			return nil, gate.OutcomeRejected
		}
		if _, err := o.Bsky.Follow(ctx, p.User); err != nil {
			return nil, outcomeOf(err)
		}
		return gate.Obj{"followed": p.User}, gate.OutcomeOk
	}
	return nil, gate.OutcomeRejected
}

// fileDescriptor is core's outbox copy of a workspace file (gate-client outbound.rs).
type fileDescriptor struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	LocalPath string `json:"local_path"`
}

var imageTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true}

const maxImageBytes = 1_000_000

func (o *Ops) images(raw []json.RawMessage) ([]bsky.Image, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if o.OutboxRoot == "" || len(raw) > bsky.MaxImages {
		return nil, errors.New("files are not available")
	}
	var out []bsky.Image
	for _, r := range raw {
		var d fileDescriptor
		if json.Unmarshal(r, &d) != nil || d.LocalPath == "" || !imageTypes[d.MediaType] || d.Size > maxImageBytes {
			return nil, errors.New("not an image descriptor")
		}
		data, err := readVerified(o.OutboxRoot, d)
		if err != nil {
			return nil, err
		}
		out = append(out, bsky.Image{Data: data, MimeType: d.MediaType, Alt: d.Name})
	}
	return out, nil
}

// readVerified reads the outbox copy and checks it is exactly what core described: a normal
// relative path, no symlinks, the same size and sha256.
func readVerified(root string, d fileDescriptor) ([]byte, error) {
	rel := filepath.Clean(d.LocalPath)
	if filepath.IsAbs(rel) || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, errors.New("bad local_path")
	}
	cur := root
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return nil, errors.New("bad outbox path")
		}
	}
	f, err := os.OpenFile(filepath.Join(root, rel), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != d.Size {
		return nil, errors.New("outbox copy does not match")
	}
	data, err := io.ReadAll(io.LimitReader(f, d.Size+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != d.Size || hex.EncodeToString(sum[:]) != d.SHA256 {
		return nil, fmt.Errorf("outbox copy does not match")
	}
	return data, nil
}
