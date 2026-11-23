package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The upstream user_message frame is capped at 64 KB. A caller prompt that
// cannot fit is uploaded out of band through the tenant attachment service and
// referenced from a short user_message instead. This mirrors the desktop
// renderer's nZ()/aZn() + lZn() flow (upload-attachment -> attachment-dlp-status
// -> qbs-event files/upload/{conversation} -> payload.attachments[].path).
const (
	attachmentPromptPrefix  = "prompt-"
	attachmentPromptSuffix  = ".txt"
	attachmentMimeType      = "text/plain"
	attachmentMinPromptSize = 8 * 1024
	// The desktop client polls the DLP verdict every 1.5 s for up to 60
	// attempts (90 s); match that budget so larger files are not cut short.
	attachmentPollInterval = 1500 * time.Millisecond
	attachmentPollAttempts = 60
)

// attachmentError marks a failure in the out-of-band path that should fall
// back to the normal frame-trimming behavior instead of failing the request.
type attachmentError struct{ err error }

func (e *attachmentError) Error() string { return e.err.Error() }
func (e *attachmentError) Unwrap() error { return e.err }

func attachErr(format string, args ...any) error {
	return &attachmentError{err: fmt.Errorf(format, args...)}
}

var attachmentHTTPClient = &http.Client{Timeout: 2 * time.Minute}

// attachmentRef is the metadata referenced from user_message.payload.attachments.
type attachmentRef struct {
	ID   string
	Path string
	Name string
	Size int
}

// attachmentPromptName returns a per-message file name so a later upload in the
// same conversation cannot be confused with an earlier prompt.txt.
func attachmentPromptName(messageID string) string {
	suffix := strings.ReplaceAll(messageID, "-", "")
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	if suffix == "" {
		suffix = "00000000"
	}
	return attachmentPromptPrefix + suffix + attachmentPromptSuffix
}

func attachmentShortText(name string) string {
	return "The user's full message for this turn is in the attached file " + name + ". Read it and respond to it."
}

// userMessageBytes returns the wire size of one user_message frame. The
// upstream limit is checked against the reassembled inner JSON, not the
// WebSocket envelope.
func userMessageBytes(payload map[string]any) (int, error) {
	body, err := json.Marshal(map[string]any{"type": "user_message", "payload": payload})
	if err != nil {
		return 0, err
	}
	return len(body), nil
}

// shouldAttachPrompt reports whether the prompt is large enough to justify an
// out-of-band upload. Even when the caller's system prompt alone is oversized,
// uploading the prompt is still worthwhile: the short reference leaves more
// room for the system prompt before fitUserMessage has to trim it.
func shouldAttachPrompt(promptSize int) bool {
	return promptSize >= attachmentMinPromptSize
}

// uploadPromptAttachment performs the three-step out-of-band upload. It
// returns the server-side path and attachment id that go on the user_message.
func uploadPromptAttachment(ctx context.Context, sess *Session, threadID, name string, data []byte) (attachmentRef, error) {
	return uploadAttachment(ctx, sess, threadID, name, attachmentMimeType, data)
}

func uploadAttachment(ctx context.Context, sess *Session, threadID, name, mimeType string, data []byte) (attachmentRef, error) {
	ref := attachmentRef{Name: name, Size: len(data)}
	if sess == nil {
		return ref, attachErr("attachment upload: no session")
	}
	base, err := tenantBase(sess.TenantURL)
	if err != nil {
		return ref, err
	}
	token := sess.jwt()
	if token == "" {
		return ref, attachErr("attachment upload: no jwt")
	}
	if len(data) == 0 {
		return ref, attachErr("attachment upload: empty prompt")
	}

	// 1. Upload the raw bytes. The tenant returns an attachment id and starts
	// the DLP scan.
	q := url.Values{}
	q.Set("name", name)
	q.Set("contentType", mimeType)
	q.Set("contentLength", strconv.Itoa(len(data)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/integration/quick-work/upload-attachment?"+q.Encode(), bytes.NewReader(data))
	if err != nil {
		return ref, attachErr("attachment upload request: %v", err)
	}
	setAttachmentHeaders(req, token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := attachmentHTTPClient.Do(req)
	if err != nil {
		return ref, attachErr("attachment upload: %v", err)
	}
	body, readErr := readLimited(resp.Body, 1<<20)
	resp.Body.Close()
	if readErr != nil {
		return ref, attachErr("attachment upload read: %v", readErr)
	}
	if resp.StatusCode >= 400 {
		return ref, attachErr("attachment upload HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	var up struct {
		AttachmentID string      `json:"attachmentId"`
		Path         string      `json:"path"`
		Files        []pathEntry `json:"files"`
	}
	if err := json.Unmarshal(body, &up); err != nil {
		return ref, attachErr("attachment upload decode: %v", err)
	}
	ref.ID = strings.TrimSpace(up.AttachmentID)
	ref.Path = firstPath(up.Path, up.Files)
	if ref.ID == "" && ref.Path == "" {
		return ref, attachErr("attachment upload returned no attachment id or path")
	}

	// 2. Wait for the DLP verdict. WARN requires an interactive acknowledgement
	// in the desktop app, so it is treated as blocked for an API relay.
	if ref.ID != "" {
		if err := waitAttachmentDLP(ctx, base, token, ref.ID); err != nil {
			return ref, err
		}
	}
	if ref.Path != "" {
		return ref, nil
	}

	// 3. Bind the attachment to the conversation workspace. Without this path
	// the model cannot see the file.
	path, err := deliverAttachment(ctx, base, token, threadID, ref.ID)
	if err != nil {
		return ref, err
	}
	ref.Path = path
	return ref, nil
}

func waitAttachmentDLP(ctx context.Context, base, token, attachmentID string) error {
	query := "/integration/quick-work/attachment-dlp-status?attachmentId=" + url.QueryEscape(attachmentID)
	for attempt := range attachmentPollAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return attachErr("attachment DLP: %v", ctx.Err())
			case <-time.After(attachmentPollInterval):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+query, nil)
		if err != nil {
			return attachErr("attachment DLP request: %v", err)
		}
		setAttachmentHeaders(req, token)
		resp, err := attachmentHTTPClient.Do(req)
		if err != nil {
			return attachErr("attachment DLP: %v", err)
		}
		body, readErr := readLimited(resp.Body, 1<<20)
		resp.Body.Close()
		if readErr != nil {
			return attachErr("attachment DLP read: %v", readErr)
		}
		if resp.StatusCode >= 400 {
			return attachErr("attachment DLP HTTP %d: %s", resp.StatusCode, snippet(body))
		}
		var out struct {
			DLPJobStatus string `json:"dlpJobStatus"`
			PolicyAction string `json:"policyAction"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return attachErr("attachment DLP decode: %v", err)
		}
		status := strings.ToUpper(strings.TrimSpace(out.DLPJobStatus))
		action := strings.ToUpper(strings.TrimSpace(out.PolicyAction))
		switch action {
		case "ALLOW":
			return nil
		case "BLOCK", "DENY", "REJECT", "WARN":
			return attachErr("attachment blocked by DLP (%s)", action)
		}
		switch status {
		case "SKIPPED", "NOT_APPLICABLE", "DISABLED":
			return nil
		case "FAILED", "ERROR":
			return attachErr("attachment DLP scan failed (%s)", status)
		case "COMPLETED":
			// The job finished without an explicit allow verdict.
			return attachErr("attachment DLP completed without an allow verdict")
		}
	}
	return attachErr("attachment DLP scan did not settle")
}

func deliverAttachment(ctx context.Context, base, token, threadID, attachmentID string) (string, error) {
	// The envelope's body is the already-decoded JSON document: the DataPlane
	// deserializes invokeAgent verbatim (buildInvokeAgentEnvelope in the desktop
	// main process parses the renderer's body string before sending it).
	event := map[string]any{
		"eventType": "invokeAgent",
		"eventBody": map[string]any{"invokeAgent": map[string]any{
			"path":        "files/upload/" + url.PathEscape(threadID),
			"queryParams": map[string]any{"_method": "POST"},
			"body":        map[string]any{"attachmentId": attachmentID},
		}},
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return "", attachErr("attachment deliver encode: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/integration/quick-work/qbs-event", bytes.NewReader(raw))
	if err != nil {
		return "", attachErr("attachment deliver request: %v", err)
	}
	setAttachmentHeaders(req, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := attachmentHTTPClient.Do(req)
	if err != nil {
		return "", attachErr("attachment deliver: %v", err)
	}
	body, readErr := readLimited(resp.Body, 2<<20)
	resp.Body.Close()
	if readErr != nil {
		return "", attachErr("attachment deliver read: %v", readErr)
	}
	if resp.StatusCode >= 400 {
		return "", attachErr("attachment deliver HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	path := extractAttachmentPath(body)
	if path == "" {
		return "", attachErr("attachment deliver returned no path: %s", snippet(body))
	}
	return path, nil
}

func tenantBase(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", attachErr("no tenant_url for attachment upload")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", attachErr("invalid tenant_url for attachment upload")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func setAttachmentHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	// The DataPlane routes are browser-facing; the desktop app always sends
	// this Origin.
	req.Header.Set("Origin", "https://quick.aws.com")
}

func firstPath(path string, files []pathEntry) string {
	if strings.TrimSpace(path) != "" {
		return path
	}
	if len(files) > 0 {
		return strings.TrimSpace(files[0].Path)
	}
	return ""
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}

func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// extractAttachmentPath pulls {path} or {files:[{path}]} out of the qbs-event
// response. The renderer receives it nested as
// eventResponse.invokeAgentResponse -> {type:"api_response", body:"<json>"}.
func extractAttachmentPath(raw []byte) string {
	var top struct {
		Path          string          `json:"path"`
		Files         []pathEntry     `json:"files"`
		EventResponse json.RawMessage `json:"eventResponse"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return ""
	}
	if p := firstPath(top.Path, top.Files); p != "" {
		return p
	}
	if len(top.EventResponse) == 0 {
		return extractFromAPIResponse(raw)
	}
	var ev struct {
		InvokeAgentResponse json.RawMessage `json:"invokeAgentResponse"`
	}
	if err := json.Unmarshal(top.EventResponse, &ev); err != nil || len(ev.InvokeAgentResponse) == 0 {
		return extractFromAPIResponse(raw)
	}
	var encoded string
	if err := json.Unmarshal(ev.InvokeAgentResponse, &encoded); err == nil {
		return extractFromAPIResponse([]byte(encoded))
	}
	return extractFromAPIResponse(ev.InvokeAgentResponse)
}

type pathEntry struct {
	Path string `json:"path"`
}

func extractFromAPIResponse(raw []byte) string {
	var api struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(raw, &api); err == nil {
		if api.Body != "" {
			if p := extractFromPathBody([]byte(api.Body)); p != "" {
				return p
			}
		}
	}
	// Some responses carry body as an object instead of a JSON string.
	var withObjectBody struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &withObjectBody); err == nil && len(withObjectBody.Body) > 0 {
		if p := extractFromPathBody(withObjectBody.Body); p != "" {
			return p
		}
	}
	var direct struct {
		Path  string      `json:"path"`
		Files []pathEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &direct); err == nil {
		if p := firstPath(direct.Path, direct.Files); p != "" {
			return p
		}
	}
	return ""
}

func extractFromPathBody(raw []byte) string {
	var v struct {
		Path  string      `json:"path"`
		Files []pathEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return firstPath(v.Path, v.Files)
}
