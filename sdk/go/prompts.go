package tracepad

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Prompts: fetched by label, cached for as long as the server says (spec 017
// #8, spec 033 #7).
//
// A prompt is read per request and changes per deploy. The store already
// says how long a label may be trusted (`Cache-Control: max-age=60`,
// docs/prompts.md), so the cache honours that and nothing else; when the
// store is away the last answer is served stale, because the reference
// application must survive a restart of its own observability. With nothing
// cached the call fails: a fallback prompt baked into the code is a prompt
// the trace cannot name.

// Message is one turn of a chat prompt.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// PromptVersion is one version of a stored prompt, what Prompt hands out:
// Text for a text prompt, Messages for a chat one. (The type is not named
// Prompt because the function is, and Go has one namespace for both.)
type PromptVersion struct {
	Name     string
	Version  int
	Type     string
	Text     string
	Messages []Message
	Labels   []string
	Config   map[string]any
}

// Compiled is a prompt with its placeholders filled: Text for a text prompt,
// Messages for a chat one — the two are different data, and the caller
// knows which it asked for.
type Compiled struct {
	Text     string
	Messages []Message
}

// Compile substitutes {name} placeholders from vars, in the text or in every
// message's content. `{{` and `}}` are literal braces; a placeholder vars
// does not name is left as it is. Nothing else: a template language is a
// product, and what the store stores is plain text.
func (p *PromptVersion) Compile(vars map[string]any) Compiled {
	if p.Messages != nil {
		messages := make([]Message, len(p.Messages))
		for i, m := range p.Messages {
			messages[i] = Message{Role: m.Role, Content: substitute(m.Content, vars)}
		}
		return Compiled{Messages: messages}
	}
	return Compiled{Text: substitute(p.Text, vars)}
}

func substitute(template string, vars map[string]any) string {
	var out strings.Builder
	for i := 0; i < len(template); i++ {
		switch c := template[i]; {
		case c == '{' && i+1 < len(template) && template[i+1] == '{':
			out.WriteByte('{')
			i++
		case c == '}' && i+1 < len(template) && template[i+1] == '}':
			out.WriteByte('}')
			i++
		case c == '{':
			end := strings.IndexByte(template[i:], '}')
			if end < 0 {
				out.WriteString(template[i:])
				return out.String()
			}
			name := template[i+1 : i+end]
			if value, ok := vars[name]; ok {
				out.WriteString(fmt.Sprint(value))
			} else {
				out.WriteString(template[i : i+end+1])
			}
			i += end
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// PromptOption configures Prompt.
type PromptOption func(*promptKey)

type promptKey struct {
	name, label string
	version     int
}

// WithLabel fetches whatever the label currently points at.
func WithLabel(label string) PromptOption { return func(k *promptKey) { k.label = label } }

// WithVersion fetches exactly that version.
func WithVersion(version int) PromptOption { return func(k *promptKey) { k.version = version } }

type promptEntry struct {
	prompt  *PromptVersion
	expires time.Time
}

var promptCache = struct {
	sync.Mutex
	entries map[promptKey]promptEntry
}{entries: map[promptKey]promptEntry{}}

// Prompt fetches a prompt by label, by version, or the latest of them.
//
// The answer is cached per (name, label | version) for as long as the
// server said. When the store is away or answers 5xx, the last answer is
// served stale with a warning; a 4xx — a moved label, a deleted prompt, a
// wrong key — is returned as the *HTTPError it is. With nothing cached the
// error is the transport's or the *HTTPError.
func Prompt(ctx context.Context, name string, opts ...PromptOption) (*PromptVersion, error) {
	key := promptKey{name: name}
	for _, opt := range opts {
		opt(&key)
	}
	promptCache.Lock()
	cached, found := promptCache.entries[key]
	promptCache.Unlock()
	if found && time.Now().Before(cached.expires) {
		return cached.prompt, nil
	}

	c, err := current()
	if err != nil {
		return nil, err
	}
	params := url.Values{}
	if key.label != "" {
		params.Set("label", key.label)
	}
	if key.version != 0 {
		params.Set("version", strconv.Itoa(key.version))
	}
	answer, err := request(ctx, c, "GET", "/api/v1/prompts/"+url.PathEscape(name), nil, params)
	if err != nil {
		if !found || isClientError(err) {
			return nil, err
		}
		// The store restarting must not take the application down; the
		// label it moved meanwhile is late by at most the window it
		// published.
		def.log().Warn("tracepad: serving a prompt from a stale cache", "name", name, "error", err)
		return cached.prompt, nil
	}
	fetched := readPrompt(object(answer))
	promptCache.Lock()
	promptCache.entries[key] = promptEntry{prompt: fetched, expires: time.Now().Add(maxAge(answer.headers))}
	promptCache.Unlock()
	return fetched, nil
}

func readPrompt(body map[string]any) *PromptVersion {
	p := &PromptVersion{Config: map[string]any{}}
	p.Name, _ = body["name"].(string)
	p.Type, _ = body["type"].(string)
	if version, ok := body["version"].(float64); ok {
		p.Version = int(version)
	}
	switch stored := body["prompt"].(type) {
	case string:
		p.Text = stored
	case []any:
		p.Messages = make([]Message, 0, len(stored))
		for _, item := range stored {
			m, _ := item.(map[string]any)
			role, _ := m["role"].(string)
			content, _ := m["content"].(string)
			p.Messages = append(p.Messages, Message{Role: role, Content: content})
		}
	}
	if labels, ok := body["labels"].([]any); ok {
		for _, label := range labels {
			if text, ok := label.(string); ok {
				p.Labels = append(p.Labels, text)
			}
		}
	}
	if config, ok := body["config"].(map[string]any); ok {
		p.Config = config
	}
	return p
}

// forgetPrompts empties the cache. For tests.
func forgetPrompts() {
	promptCache.Lock()
	defer promptCache.Unlock()
	promptCache.entries = map[promptKey]promptEntry{}
}
