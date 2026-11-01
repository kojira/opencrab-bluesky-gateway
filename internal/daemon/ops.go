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
	"strings"
	"syscall"

	"github.com/kojira/opencrab-bluesky-gateway/internal/bsky"
	"github.com/kojira/opencrab-bluesky-gateway/internal/gate"
)

const endTurn = "結果は返らない。この呼び出しだけではターンは終わらない。これで終えるなら、同じ応答の最後の行に NO_REPLY だけを書く。This call returns nothing. It does not end the turn by itself; to end the turn, write NO_REPLY alone on the final line of this same response."

// Operations is the hello declaration, sorted by name as core requires. withFiles adds the
// `files` field (workspace images) to post and reply.
func Operations(withFiles bool) []any {
	decl := func(name, desc string, input gate.Obj, utterance bool, sharing string) gate.Obj {
		dispatch, effect := "background", "state_change"
		if utterance {
			dispatch, effect = "utterance", "utterance"
		}
		return gate.Obj{
			"name":            name,
			"description":     desc,
			"input_schema":    input,
			"output_schema":   nil,
			"callback_schema": nil,
			"authorization":   gate.Obj{"allowed_callers": []any{"co_agent", "guest", "owner", "trusted"}},
			"dispatch":        dispatch,
			"sub_engine":      "not_exposed",
			"sharing":         sharing,
			"effect":          effect,
		}
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
		"event": ref("返信先の投稿の短縮参照（例 e7）"),
		"text":  str("返信本文"),
	}}
	if withFiles {
		post["properties"].(gate.Obj)["files"] = files
		reply["properties"].(gate.Obj)["files"] = files
	}
	return []any{
		decl("follow", "相手をフォローする。user に会話の u番号、または handle（例 alice.bsky.social）か did を渡す。",
			gate.Obj{"type": "object", "required": []any{"user"}, "properties": gate.Obj{"user": ref("フォローする相手（u番号、handle、did）")}},
			false, "agent_bound"),
		decl("like", "投稿にいいねする。event に会話の e番号。"+endTurn,
			gate.Obj{"type": "object", "required": []any{"event"}, "properties": gate.Obj{"event": ref("対象の投稿の短縮参照（例 e7）")}},
			true, "conversation_bound"),
		decl("post", "返信ではない新しい投稿をする。text に本文。ツールを使わずに本文を書いても同じく新規投稿になる。"+endTurn, post, true, "conversation_bound"),
		decl("reply", "reply を呼ぶ応答では、地の文に何も書かない（最後の行の NO_REPLY だけは書いてよい）。地の文は別の投稿になるので、返信が二重になる。投稿に返信する。event に会話の e番号、text に返信本文。"+endTurn, reply, true, "conversation_bound"),
		decl("repost", "投稿をリポストする。event に会話の e番号。"+endTurn,
			gate.Obj{"type": "object", "required": []any{"event"}, "properties": gate.Obj{"event": ref("対象の投稿の短縮参照（例 e7）")}},
			true, "conversation_bound"),
	}
}

// Poster is the part of the Bluesky client the operations use (a stub in tests).
type Poster interface {
	Post(ctx context.Context, text, replyTo string, images []bsky.Image) ([]string, error)
	Like(ctx context.Context, uri string) (string, error)
	Repost(ctx context.Context, uri string) (string, error)
	Follow(ctx context.Context, who string) (string, error)
}

// Ops runs core's invokes and says for one bot.
type Ops struct {
	Bsky       Poster
	OutboxRoot string
}

func (o *Ops) Say(ctx context.Context, _ string, text, replyTarget string) error {
	_, err := o.Bsky.Post(ctx, text, replyTarget, nil)
	return err
}

func outcomeOf(err error) gate.Outcome {
	var apiErr *bsky.APIError
	if errors.As(err, &apiErr) && apiErr.Permanent() {
		return gate.OutcomeRejected
	}
	return gate.OutcomeIndeterminate
}

func (o *Ops) Invoke(ctx context.Context, _ string, operation string, raw json.RawMessage) (any, gate.Outcome) {
	var p struct {
		Text  string            `json:"text"`
		Event string            `json:"event"`
		User  string            `json:"user"`
		Files []json.RawMessage `json:"files"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil, gate.OutcomeRejected
	}
	switch operation {
	case "post", "reply":
		if operation == "reply" && !strings.HasPrefix(p.Event, "at://") {
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
		if !strings.HasPrefix(p.Event, "at://") {
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
