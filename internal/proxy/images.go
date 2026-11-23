package proxy

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"
)

type imageInput struct {
	MIME, Name string
	Data       []byte
}

func imageInputs(messages []Message) ([]imageInput, error) {
	result := []imageInput{}
	total := 0
	for _, msg := range messages {
		parts, ok := msg.Content.([]any)
		if !ok {
			continue
		}
		for _, value := range parts {
			part, ok := value.(map[string]any)
			if !ok {
				continue
			}
			var uri string
			switch part["type"] {
			case "image_url":
				uri = imagePartURL(part)
			case "image":
				source, _ := part["source"].(map[string]any)
				if source["type"] != "base64" {
					return nil, fmt.Errorf("image source must contain base64 data")
				}
				mime, _ := source["media_type"].(string)
				data, _ := source["data"].(string)
				uri = "data:" + mime + ";base64," + data
			default:
				continue
			}
			if len(result) >= 8 {
				return nil, fmt.Errorf("at most 8 images are supported per request")
			}
			if !strings.HasPrefix(uri, "data:") {
				return nil, fmt.Errorf("remote image fetching is disabled to prevent SSRF; send a base64 image data URL")
			}
			header, encoded, ok := strings.Cut(strings.TrimPrefix(uri, "data:"), ",")
			if !ok {
				return nil, fmt.Errorf("invalid image data URL")
			}
			mime, ok := strings.CutSuffix(header, ";base64")
			if !ok {
				return nil, fmt.Errorf("image data URL must use base64 encoding")
			}
			ext := ""
			switch mime {
			case "image/png":
				ext = "png"
			case "image/jpeg":
				ext = "jpeg"
			case "image/gif":
				ext = "gif"
			default:
				return nil, fmt.Errorf("supported image formats are PNG, JPEG and GIF")
			}
			if len(encoded) > base64.StdEncoding.EncodedLen(5<<20) {
				return nil, fmt.Errorf("each image must not exceed 5 MiB")
			}
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, fmt.Errorf("invalid image base64 data")
			}
			cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil || format != ext || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 8192 || cfg.Height > 8192 {
				return nil, fmt.Errorf("invalid image, mismatched media type or dimensions larger than 8192 pixels")
			}
			total += len(data)
			if total > 20<<20 {
				return nil, fmt.Errorf("combined image size must not exceed 20 MiB")
			}
			result = append(result, imageInput{MIME: mime, Name: fmt.Sprintf("image-%d.%s", len(result)+1, ext), Data: data})
		}
	}
	return result, nil
}
