package bsky

import (
	"encoding/binary"
	"errors"
)

// MP4Info is what a post needs from an mp4: the picture size (for the embed's aspect ratio) and
// the length.
type MP4Info struct {
	Width, Height   int
	DurationSeconds float64
}

// ReadMP4Info reads the movie header (mvhd: duration) and the first video track header (tkhd with
// a picture size) of an mp4. It only walks boxes; it does not decode media.
func ReadMP4Info(data []byte) (MP4Info, error) {
	var info MP4Info
	moov, ok := findBox(data, "moov")
	if !ok {
		return info, errors.New("not an mp4 (no moov box)")
	}
	if mvhd, ok := findBox(moov, "mvhd"); ok && len(mvhd) >= 20 {
		var timescale, duration uint64
		if mvhd[0] == 1 && len(mvhd) >= 32 {
			timescale = uint64(binary.BigEndian.Uint32(mvhd[20:24]))
			duration = binary.BigEndian.Uint64(mvhd[24:32])
		} else {
			timescale = uint64(binary.BigEndian.Uint32(mvhd[12:16]))
			duration = uint64(binary.BigEndian.Uint32(mvhd[16:20]))
		}
		if timescale > 0 {
			info.DurationSeconds = float64(duration) / float64(timescale)
		}
	}
	forEachBox(moov, func(typ string, body []byte) {
		if typ != "trak" || info.Width > 0 {
			return
		}
		tkhd, ok := findBox(body, "tkhd")
		if !ok || len(tkhd) < 84 {
			return
		}
		// width and height are the last 8 bytes of tkhd, 16.16 fixed point.
		w := int(binary.BigEndian.Uint32(tkhd[len(tkhd)-8:]) >> 16)
		h := int(binary.BigEndian.Uint32(tkhd[len(tkhd)-4:]) >> 16)
		if w > 0 && h > 0 {
			info.Width, info.Height = w, h
		}
	})
	if info.Width == 0 {
		return info, errors.New("mp4 has no video track")
	}
	return info, nil
}

// forEachBox calls fn for each top-level box of data (type and body).
func forEachBox(data []byte, fn func(typ string, body []byte)) {
	for len(data) >= 8 {
		size := uint64(binary.BigEndian.Uint32(data[0:4]))
		typ := string(data[4:8])
		header := uint64(8)
		if size == 1 {
			if len(data) < 16 {
				return
			}
			size = binary.BigEndian.Uint64(data[8:16])
			header = 16
		} else if size == 0 {
			size = uint64(len(data))
		}
		if size < header || size > uint64(len(data)) {
			return
		}
		fn(typ, data[header:size])
		data = data[size:]
	}
}

func findBox(data []byte, want string) ([]byte, bool) {
	var found []byte
	ok := false
	forEachBox(data, func(typ string, body []byte) {
		if !ok && typ == want {
			found, ok = body, true
		}
	})
	return found, ok
}
