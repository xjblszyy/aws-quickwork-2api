package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// conversation keeps the upstream thread used for one client conversation and
// the client messages already forwarded to it.
type conversation struct {
	mu           sync.Mutex
	threadID     string
	accountID    int64
	messages     []Message
	systemPrompt string
	lastUsed     atomic.Int64
	users        atomic.Int64
}

func (c *conversation) touch() {
	c.lastUsed.Store(time.Now().UnixNano())
}

func (c *conversation) idle() time.Duration {
	n := c.lastUsed.Load()
	if n == 0 {
		return 0
	}
	return time.Since(time.Unix(0, n))
}

func (c *conversation) reset() {
	c.threadID = ""
	c.accountID = 0
	c.messages = nil
	c.systemPrompt = ""
}

// delta returns the messages added since the last forwarded turn. A false
// result means the history was edited or there is nothing new, so the caller
// must start a fresh upstream session with the full transcript.
func (c *conversation) delta(msgs []Message) ([]Message, bool) {
	if c.threadID == "" || len(c.messages) == 0 {
		return nil, false
	}
	if len(msgs) <= len(c.messages) {
		return nil, false
	}
	for i := range c.messages {
		if !reflect.DeepEqual(c.messages[i], msgs[i]) {
			return nil, false
		}
	}
	return msgs[len(c.messages):], true
}

type conversationStore struct {
	mu      sync.Mutex
	entries map[string]*conversation
	ttl     time.Duration
	max     int
}

func newConversationStore(ttl time.Duration, max int) *conversationStore {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if max < 1 {
		max = 256
	}
	return &conversationStore{entries: make(map[string]*conversation), ttl: ttl, max: max}
}

// acquire locks one conversation for the duration of a request. Expired and
// least-recently-used conversations are dropped first. The store lock is
// released before waiting on a conversation so one busy chat cannot stall
// unrelated conversations.
func (s *conversationStore) acquire(key string) *conversation {
	s.mu.Lock()
	for k, c := range s.entries {
		if c.users.Load() == 0 && c.idle() > s.ttl {
			delete(s.entries, k)
		}
	}
	if s.entries[key] == nil && len(s.entries) >= s.max {
		var oldestKey string
		var oldest time.Time
		for k, c := range s.entries {
			if c.users.Load() > 0 {
				continue
			}
			used := time.Unix(0, c.lastUsed.Load())
			if oldestKey == "" || used.Before(oldest) {
				oldestKey, oldest = k, used
			}
		}
		if oldestKey != "" {
			delete(s.entries, oldestKey)
		}
	}
	c := s.entries[key]
	if c == nil {
		c = &conversation{}
		if len(s.entries) < s.max {
			s.entries[key] = c
		}
	}
	c.users.Add(1)
	s.mu.Unlock()

	c.mu.Lock()
	c.touch()
	return c
}

func (s *conversationStore) release(c *conversation) {
	c.touch()
	c.mu.Unlock()
	c.users.Add(-1)
}

// conversationKey identifies one OpenAI conversation. A client-provided
// X-Conversation-Id or X-Thread-Id wins; otherwise the full system prompt
// (including tool definitions) plus the first user message is used so the key
// stays stable while the transcript grows.
func conversationKey(r *http.Request, req *ChatRequest, fullSystemPrompt string) string {
	h := sha256.New()
	h.Write([]byte(tokenFromRequest(r)))
	h.Write([]byte{0})
	h.Write([]byte(req.Model))
	h.Write([]byte{0})
	h.Write([]byte(r.URL.Path))
	h.Write([]byte{0})
	for _, header := range []string{"X-Conversation-Id", "X-Thread-Id"} {
		if id := strings.TrimSpace(r.Header.Get(header)); id != "" {
			h.Write([]byte(header + ":" + id))
			return "h:" + hex.EncodeToString(h.Sum(nil))
		}
	}
	h.Write([]byte(fullSystemPrompt))
	for _, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" || role == "developer" {
			continue
		}
		h.Write([]byte{0})
		h.Write([]byte(role))
		h.Write([]byte{0})
		h.Write([]byte(contentText(m.Content)))
		return "c:" + hex.EncodeToString(h.Sum(nil))
	}
	for _, m := range req.Messages {
		h.Write([]byte{0})
		h.Write([]byte(m.Role))
		h.Write([]byte(contentText(m.Content)))
	}
	return "c:" + hex.EncodeToString(h.Sum(nil))
}
