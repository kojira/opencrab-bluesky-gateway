package bsky

import "encoding/json"

type blobRef struct {
	Ref struct {
		Link string `json:"$link"`
	} `json:"ref"`
	MimeType string `json:"mimeType"`
}

type imagesEmbed struct {
	Type   string `json:"$type"`
	Images []struct {
		Image blobRef `json:"image"`
	} `json:"images"`
	Media *imagesEmbed `json:"media"`
}

// ImageURLs gives the https CDN URLs of a post record's images (images or recordWithMedia). The
// one place that turns a post's images into URLs: delivered saids, read_* and resolve use it.
func ImageURLs(authorDID string, embed json.RawMessage) []string {
	if len(embed) == 0 {
		return nil
	}
	var e imagesEmbed
	if json.Unmarshal(embed, &e) != nil {
		return nil
	}
	imgs := e.Images
	if e.Media != nil {
		imgs = e.Media.Images
	}
	var out []string
	for _, img := range imgs {
		if img.Image.Ref.Link == "" {
			continue
		}
		out = append(out, "https://cdn.bsky.app/img/feed_fullsize/plain/"+authorDID+"/"+img.Image.Ref.Link+"@jpeg")
	}
	return out
}
