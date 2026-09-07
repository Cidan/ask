package tools

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	_ "image/gif"
	_ "image/jpeg"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// readImageFile handles `read` on an image file. It decodes the image,
// normalizes it to PNG (so every provider accepts it), and queues it on the
// session ImageSink; the ImageInjectionHook then feeds the pixels to a
// vision-capable model before the next model call. The tool result itself is
// only the text description — tool results are text/JSON on every provider, so
// the image can reach the model only as an injected user image part.
func readImageFile(env *ToolEnv, path string) (ReadResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ReadResult{}, fmt.Errorf("open %s: %v", path, err)
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return ReadResult{Content: fmt.Sprintf("%s is a %s image but could not be decoded for display: %v", path, ext, err)}, nil
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	if env == nil || env.ImageSink == nil {
		return ReadResult{Content: fmt.Sprintf("%s is a %d×%d %s image; image display is unavailable in this session.", path, w, h, format)}, nil
	}
	if env.SupportsImages != nil && !env.SupportsImages() {
		return ReadResult{Content: fmt.Sprintf("%s is a %d×%d %s image; the active model has no vision, so it cannot be shown.", path, w, h, format)}, nil
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return ReadResult{}, fmt.Errorf("encode %s for display: %v", path, err)
	}
	env.ImageSink.Add(PendingImage{
		Data:    buf.Bytes(),
		MIME:    "image/png",
		Caption: fmt.Sprintf("%s (%d×%d):", path, w, h),
	})
	return ReadResult{Content: fmt.Sprintf("Displaying %s — a %d×%d %s image (shown below).", path, w, h, format)}, nil
}
