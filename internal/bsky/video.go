package bsky

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Video limits and service (sources: app.bsky.embed.video lexicon — video/mp4 only, maxSize
// 300,000,000 bytes; social-app src/lib/constants.ts — VIDEO_SERVICE https://video.bsky.app,
// VIDEO_MAX_DURATION_MS 10 minutes; docs tutorials/video.mdx — upload to the video service with a
// service token for the PDS, aud did:web:<pds host>, lxm com.atproto.repo.uploadBlob, then poll
// app.bsky.video.getJobStatus until it returns the blob).
const (
	VideoMIME           = "video/mp4"
	MaxVideoBytes       = 300_000_000
	MaxVideoSeconds     = 10 * 60
	DefaultVideoService = "https://video.bsky.app"
	// VideoJobWait bounds how long a post waits for the video service to finish processing.
	VideoJobWait = 5 * time.Minute
)

// IsVideo reports whether a mime type is a video this gateway can post.
func IsVideo(mimeType string) bool { return mimeType == VideoMIME }

// MediaError is a refusal the model can act on (too big, too long, mixed with images, ...).
type MediaError struct{ Reason string }

func (e *MediaError) Error() string { return e.Reason }

func (c *Client) videoService() string {
	if c.VideoService != "" {
		return strings.TrimRight(c.VideoService, "/")
	}
	return DefaultVideoService
}

func (c *Client) pdsBase() string {
	if c.OAuth != nil {
		return strings.TrimRight(c.OAuth.Data.HostURL, "/")
	}
	return c.service()
}

type jobStatus struct {
	JobID   string          `json:"jobId"`
	State   string          `json:"state"`
	Blob    json.RawMessage `json:"blob"`
	Error   string          `json:"error"`
	Message string          `json:"message"`
}

// CheckVideo validates a video against Bluesky's limits and gives its aspect ratio.
func CheckVideo(v Image) (MP4Info, error) {
	if !IsVideo(v.MimeType) {
		return MP4Info{}, &MediaError{fmt.Sprintf("動画は mp4（video/mp4）だけ投稿できます（%s は不可）", v.MimeType)}
	}
	if len(v.Data) > MaxVideoBytes {
		return MP4Info{}, &MediaError{fmt.Sprintf("動画が大きすぎます（%d バイト）。Bluesky の上限は %d バイト（300MB）です", len(v.Data), MaxVideoBytes)}
	}
	info, err := ReadMP4Info(v.Data)
	if err != nil {
		return MP4Info{}, &MediaError{"動画を読めません: " + err.Error()}
	}
	if info.DurationSeconds > MaxVideoSeconds {
		return MP4Info{}, &MediaError{fmt.Sprintf("動画が長すぎます（%.0f 秒）。Bluesky の上限は %d 秒（10分）です", info.DurationSeconds, MaxVideoSeconds)}
	}
	return info, nil
}

// UploadVideo sends an mp4 to the video service and waits (bounded) for the processed blob.
func (c *Client) UploadVideo(ctx context.Context, v Image) (Blob, error) {
	did, err := c.SelfDID(ctx)
	if err != nil {
		return nil, err
	}
	pds, err := url.Parse(c.pdsBase())
	if err != nil {
		return nil, err
	}
	var auth struct {
		Token string `json:"token"`
	}
	q := url.Values{
		"aud": {"did:web:" + pds.Hostname()},
		"lxm": {"com.atproto.repo.uploadBlob"},
		"exp": {fmt.Sprint(time.Now().Add(30 * time.Minute).Unix())},
	}
	if err := c.call(ctx, http.MethodGet, "com.atproto.server.getServiceAuth", q, nil, &auth); err != nil {
		return nil, err
	}
	name := v.Alt
	if name == "" {
		name = "video.mp4"
	}
	up := url.Values{"did": {did}, "name": {name}}
	var job jobStatus
	err = c.serviceCall(ctx, http.MethodPost, "app.bsky.video.uploadVideo", up, rawBody{v.Data, VideoMIME}, auth.Token, &job)
	var apiErr *APIError
	if err != nil && !(errors.As(err, &apiErr) && apiErr.Code == "already_exists") {
		return nil, err
	}
	deadline := time.Now().Add(VideoJobWait)
	for {
		if len(job.Blob) > 0 && string(job.Blob) != "null" {
			return job.Blob, nil
		}
		if job.State == "JOB_STATE_FAILED" {
			return nil, &MediaError{"動画の処理に失敗しました: " + firstNonEmpty(job.Message, job.Error, "unknown")}
		}
		if job.JobID == "" {
			return nil, errors.New("bsky: video service returned no job")
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("bsky: video processing did not finish within %s", VideoJobWait)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.videoPoll()):
		}
		var st struct {
			JobStatus jobStatus `json:"jobStatus"`
		}
		if err := c.serviceCall(ctx, http.MethodGet, "app.bsky.video.getJobStatus", url.Values{"jobId": {job.JobID}}, nil, "", &st); err != nil {
			return nil, err
		}
		job = st.JobStatus
	}
}

func (c *Client) videoPoll() time.Duration {
	if c.VideoPollInterval > 0 {
		return c.VideoPollInterval
	}
	return time.Second
}

// serviceCall is an XRPC call to the video service with a service token (not the PDS session).
func (c *Client) serviceCall(ctx context.Context, method, nsid string, query url.Values, body any, token string, out any) error {
	return c.send(ctx, c.videoService(), method, nsid, query, body, token, nil, out)
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}
