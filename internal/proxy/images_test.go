package proxy

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"testing"
)

func TestImageInputsAreRealValidatedAttachments(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data.Bytes())
	msgs := []Message{{Role: "user", Content: []any{map[string]any{"type": "text", "text": "what is in this picture?"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}}}}}
	images, err := imageInputs(msgs)
	if err != nil || len(images) != 1 || images[0].MIME != "image/png" || !bytes.Equal(images[0].Data, data.Bytes()) {
		t.Fatalf("images=%+v err=%v", images, err)
	}
	for _, url := range []string{"http://127.0.0.1/private", "https://example.com/image.png", "data:image/png;base64,invalid", "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data.Bytes())} {
		_, err := imageInputs([]Message{{Role: "user", Content: []any{map[string]any{"type": "image_url", "image_url": url}}}})
		if err == nil {
			t.Fatalf("unsafe or invalid image accepted: %s", url)
		}
	}
}
