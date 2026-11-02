package api

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"kwiqtest/internal/model"
)

// Media is accepted only as a local-drive file upload. There is no endpoint
// that fetches a remote URL, which is what keeps a live session immune to a
// third-party image host going down or expiring a link mid-quiz.

var imageTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

var videoTypes = map[string]string{
	"video/mp4":       ".mp4",
	"video/webm":      ".webm",
	"video/quicktime": ".mov",
}

// maxDimension is the longest edge kept when re-encoding an uploaded photo.
// Phone cameras produce 4000px images; a question card never needs more than
// this, and shrinking it is the single biggest win for participants on mobile
// data, who all download the same asset at the same moment.
const maxDimension = 1600

func (s *Server) uploadMedia(w http.ResponseWriter, r *http.Request) {
	limit := s.cfg.MaxVideoBytes
	if limit < s.cfg.MaxImageBytes {
		limit = s.cfg.MaxImageBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("upload too large (limit %s)", humanBytes(limit)))
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "no file provided")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read upload")
		return
	}
	if len(data) == 0 {
		writeErr(w, http.StatusBadRequest, "the selected file is empty")
		return
	}

	// Trust sniffed bytes over the client-declared type, then cross-check the
	// extension so a renamed .exe cannot masquerade as a picture.
	mime := sniffType(data, header.Filename)
	ext, isImage := imageTypes[mime]
	vext, isVideo := videoTypes[mime]
	if isVideo {
		ext = vext
	}
	if !isImage && !isVideo {
		writeErr(w, http.StatusUnsupportedMediaType,
			"unsupported file type — use JPG, PNG, GIF or WebP images, or MP4/WebM video")
		return
	}

	kind := model.MediaImage
	max := s.cfg.MaxImageBytes
	if isVideo {
		kind, max = model.MediaVideo, s.cfg.MaxVideoBytes
	}
	if int64(len(data)) > max {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"%s is %s — the limit for %ss is %s",
			header.Filename, humanBytes(int64(len(data))), kind, humanBytes(max)))
		return
	}

	width, height := 0, 0
	if isImage {
		// JPEG and PNG are re-encoded down to a sane size. GIF and WebP pass
		// through untouched: re-encoding would drop GIF animation, and WebP
		// has no stdlib decoder.
		if mime == "image/jpeg" || mime == "image/png" {
			if out, oe, ow, oh, err := compressImage(data, mime); err == nil {
				data, ext, width, height = out, oe, ow, oh
				mime = map[string]string{".jpg": "image/jpeg", ".png": "image/png"}[oe]
			}
		}
		if width == 0 {
			if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
				width, height = cfg.Width, cfg.Height
			}
		}
	}

	id, err := s.st.SaveMedia(ext, data)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save the upload")
		return
	}
	writeJSON(w, http.StatusCreated, &model.Media{
		ID: id, Kind: kind, Mime: mime, Name: filepath.Base(header.Filename),
		Size: int64(len(data)), Width: width, Heigh: height,
	})
}

// sniffType identifies content from magic bytes, with an extension fallback for
// the container formats http.DetectContentType reports only as octet-stream.
func sniffType(data []byte, filename string) string {
	t := strings.Split(http.DetectContentType(data), ";")[0]
	if _, ok := imageTypes[t]; ok {
		return t
	}
	if _, ok := videoTypes[t]; ok {
		return t
	}
	if len(data) > 12 && string(data[4:8]) == "ftyp" {
		return "video/mp4"
	}
	if len(data) > 4 && bytes.Equal(data[:4], []byte{0x1A, 0x45, 0xDF, 0xA3}) {
		return "video/webm"
	}
	if len(data) > 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return "image/webp"
	}
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".webp":
		return "image/webp"
	}
	return t
}

// compressImage downscales to maxDimension and re-encodes. Images carrying
// transparency stay PNG; everything else becomes JPEG, which is dramatically
// smaller for photographs.
func compressImage(data []byte, mime string) (out []byte, ext string, w, h int, err error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", 0, 0, err
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dw, dh := sw, sh
	if sw > maxDimension || sh > maxDimension {
		if sw >= sh {
			dw = maxDimension
			dh = sh * maxDimension / sw
		} else {
			dh = maxDimension
			dw = sw * maxDimension / sh
		}
	}
	img := src
	if dw != sw || dh != sh {
		img = downscale(src, dw, dh)
	}

	var buf bytes.Buffer
	if mime == "image/png" && hasAlpha(img) {
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		if err := enc.Encode(&buf, img); err != nil {
			return nil, "", 0, 0, err
		}
		ext = ".png"
	} else {
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 82}); err != nil {
			return nil, "", 0, 0, err
		}
		ext = ".jpg"
	}
	// Never let "compression" make a file bigger. Some sources — screenshots,
	// flat graphics, anything already well-optimised — encode smaller as the
	// original PNG than as a re-encoded JPEG, and shipping the larger file to
	// every phone in the room would be the opposite of the point.
	if buf.Len() >= len(data) {
		return nil, "", 0, 0, fmt.Errorf("re-encoding gained nothing")
	}
	return buf.Bytes(), ext, dw, dh, nil
}

// downscale performs a box-filter reduction: each destination pixel averages
// the source pixels it covers. It is a handful of lines, has no dependencies,
// and for downsizing looks as good as fancier kernels.
func downscale(src image.Image, dw, dh int) image.Image {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		y0 := sb.Min.Y + y*sh/dh
		y1 := sb.Min.Y + (y+1)*sh/dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < dw; x++ {
			x0 := sb.Min.X + x*sw/dw
			x1 := sb.Min.X + (x+1)*sw/dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var rs, gs, bs, as, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					r, g, b, a := src.At(sx, sy).RGBA()
					rs += uint64(r)
					gs += uint64(g)
					bs += uint64(b)
					as += uint64(a)
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.Set(x, y, color.RGBA64{
				R: uint16(rs / n), G: uint16(gs / n), B: uint16(bs / n), A: uint16(as / n),
			})
		}
	}
	return dst
}

func hasAlpha(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return !o.Opaque()
	}
	b := img.Bounds()
	rgba := image.NewRGBA(b)
	draw.Draw(rgba, b, img, b.Min, draw.Src)
	return !rgba.Opaque()
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// serveMedia streams an uploaded asset. http.ServeContent gives us range
// requests for free, which mobile Safari requires before it will play video.
func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request) {
	id := filepath.Base(r.PathValue("id"))
	if id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		http.NotFound(w, r)
		return
	}
	path := s.st.MediaPath(id)
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	if ct := contentTypeForExt(filepath.Ext(id)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// Media is immutable: its filename contains a random id, so a new upload
	// always gets a new URL and this can be cached hard.
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, id, info.ModTime(), f)
}

func contentTypeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	}
	return ""
}

// registered so image.Decode recognises GIF alongside JPEG and PNG
var _ = gif.Decode
