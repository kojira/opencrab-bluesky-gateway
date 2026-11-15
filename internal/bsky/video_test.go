package bsky

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func box(typ string, body []byte) []byte {
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

// testMP4 is a minimal mp4: ftyp + moov(mvhd, trak(tkhd)) with the given size and length.
func testMP4(w, h int, seconds uint32, pad int) []byte {
	mvhd := make([]byte, 100)
	binary.BigEndian.PutUint32(mvhd[12:], 1000)
	binary.BigEndian.PutUint32(mvhd[16:], seconds*1000)
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[76:], uint32(w)<<16)
	binary.BigEndian.PutUint32(tkhd[80:], uint32(h)<<16)
	moov := box("moov", append(box("mvhd", mvhd), box("trak", box("tkhd", tkhd))...))
	out := append(box("ftyp", []byte("isom\x00\x00\x02\x00isomiso2mp41")), moov...)
	return append(out, box("mdat", make([]byte, pad))...)
}

type videoStub struct {
	mu       sync.Mutex
	uploaded []byte
	auth     string
	polls    int
}

func newVideoStub(t *testing.T, pollsBeforeDone int) (*videoStub, string) {
	v := &videoStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		defer v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/xrpc/") {
		case "app.bsky.video.uploadVideo":
			v.uploaded, _ = io.ReadAll(r.Body)
			v.auth = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]any{"jobId": "job1", "did": "did:plc:bot", "state": "JOB_STATE_CREATED"})
		case "app.bsky.video.getJobStatus":
			v.polls++
			st := map[string]any{"jobId": "job1", "did": "did:plc:bot", "state": "JOB_STATE_ENCODING"}
			if v.polls > pollsBeforeDone {
				st["state"] = "JOB_STATE_COMPLETED"
				st["blob"] = map[string]any{"$type": "blob", "ref": map[string]string{"$link": "bafyvid"}, "mimeType": "video/mp4", "size": 10}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jobStatus": st})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return v, srv.URL
}

func TestMP4IsUploadedToTheVideoServiceAndEmbedded(t *testing.T) {
	s, c := newStub(t)
	v, url := newVideoStub(t, 2)
	c.VideoService, c.VideoPollInterval = url, time.Millisecond
	mp4 := testMP4(1280, 720, 6, 1000)
	if _, err := c.Post(context.Background(), "動画", "", []Image{{Data: mp4, MimeType: "video/mp4", Alt: "breakfast_dance.mp4"}}); err != nil {
		t.Fatal(err)
	}
	if string(v.uploaded) != string(mp4) || v.auth != "Bearer svc-token" || v.polls != 3 {
		t.Fatalf("upload %d bytes auth %q polls %d", len(v.uploaded), v.auth, v.polls)
	}
	if len(s.uploads) != 0 {
		t.Fatal("a video must not go through uploadBlob")
	}
	embed := record(t, s, 0)["embed"].(map[string]any)
	if embed["$type"] != "app.bsky.embed.video" || embed["alt"] != "breakfast_dance.mp4" {
		t.Fatalf("embed %v", embed)
	}
	ar := embed["aspectRatio"].(map[string]any)
	if ar["width"] != 1280.0 || ar["height"] != 720.0 {
		t.Fatalf("aspect %v", ar)
	}
	if embed["video"].(map[string]any)["ref"].(map[string]any)["$link"] != "bafyvid" {
		t.Fatalf("video blob %v", embed["video"])
	}
}

func TestImagesAndVideoTogetherAreRejected(t *testing.T) {
	s, c := newStub(t)
	_, err := c.Post(context.Background(), "x", "", []Image{
		{Data: testMP4(640, 480, 3, 10), MimeType: "video/mp4"},
		{Data: []byte("png"), MimeType: "image/png"},
	})
	var me *MediaError
	if !errors.As(err, &me) || !strings.Contains(me.Reason, "動画1本") || len(s.created) != 0 {
		t.Fatalf("err %v created %d", err, len(s.created))
	}
}

func TestOversizedVideoIsRejectedWithTheLimit(t *testing.T) {
	s, c := newStub(t)
	_, err := c.Post(context.Background(), "x", "", []Image{{Data: testMP4(640, 480, 11*60, 10), MimeType: "video/mp4"}})
	var me *MediaError
	if !errors.As(err, &me) || !strings.Contains(me.Reason, "600 秒") || len(s.created) != 0 {
		t.Fatalf("err %v", err)
	}
	if err := CheckMedia([]Image{{Data: make([]byte, MaxVideoBytes+1), MimeType: "video/mp4"}}); err == nil || !strings.Contains(err.Error(), "300000000") {
		t.Fatalf("size err %v", err)
	}
}
