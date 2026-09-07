package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// Prompts (spec 003): versions are append-only, labels move between them, and
// `latest` is computed rather than stored (#10, #11, #12).

// promptCacheControl is sent on every prompt read (#14). Sixty seconds bounds
// how long a label move takes to reach a client that caches; version-pinned
// reads are immutable in practice and a client that pins may cache them for
// longer on its own.
const promptCacheControl = "max-age=60"

// promptVersionRequest is the wire shape of a new version. Only `type` needs
// a pointer: whether the client stated it decides what the write transaction
// checks (#10). For the rest, an absent field and an empty one mean the same
// thing and are stored the same way.
type promptVersionRequest struct {
	Type   *string         `json:"type"`
	Prompt json.RawMessage `json:"prompt"`
	Config json.RawMessage `json:"config"`
	// ExpectVersion is the optimistic append of spec 021 #14: the version
	// the author believed the name was at, 0 for a name they believe is
	// new. Absent means "append to whatever is there", which is what every
	// client did before the field existed.
	ExpectVersion *int     `json:"expect_version"`
	CommitMessage string   `json:"commit_message"`
	Labels        []string `json:"labels"`
}

type promptResponse struct {
	Name          string          `json:"name"`
	Version       int             `json:"version"`
	Type          string          `json:"type"`
	Prompt        json.RawMessage `json:"prompt"`
	Config        json.RawMessage `json:"config,omitempty"`
	CommitMessage string          `json:"commit_message,omitempty"`
	Labels        []string        `json:"labels"`
	CreatedAt     string          `json:"created_at"`
}

type promptVersionSummaryResponse struct {
	Version       int      `json:"version"`
	CommitMessage string   `json:"commit_message,omitempty"`
	Labels        []string `json:"labels"`
	CreatedAt     string   `json:"created_at"`
}

type promptVersionListResponse struct {
	Versions []promptVersionSummaryResponse `json:"versions"`
	// Labels is every label of the name with the version it points at —
	// the whole map, not the part of it that happens to be on this page
	// (spec 021 #12).
	Labels     map[string]int `json:"labels"`
	NextCursor *string        `json:"next_cursor"`
	PrevCursor *string        `json:"prev_cursor"`
}

type promptListResponse struct {
	Prompts    []promptSummaryResponse `json:"prompts"`
	NextCursor *string                 `json:"next_cursor"`
	PrevCursor *string                 `json:"prev_cursor"`
}

type promptSummaryResponse struct {
	Name          string         `json:"name"`
	Type          string         `json:"type"`
	LatestVersion int            `json:"latest_version"`
	Labels        map[string]int `json:"labels"`
	UpdatedAt     string         `json:"updated_at"`
}

type labelResponse struct {
	Label   string `json:"label"`
	Version int    `json:"version"`
}

// handleCreatePromptVersion appends a version to a name. The number it gets is
// assigned inside the write transaction, so parallel creates for one name
// produce 1..N with no gaps and no duplicates (#9, #10).
func (s *Server) handleCreatePromptVersion(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	name, ok := promptName(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request promptVersionRequest
	if !s.readJSON(w, r, &request) {
		return
	}

	write, err := request.validate(project.ID, name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.submit(w, r, write) {
		return
	}

	// Rendered from the version the commit produced, through the one
	// function that renders a prompt, so a created version and a fetched
	// one cannot drift apart in shape.
	writeJSON(w, http.StatusCreated, renderPrompt(&store.PromptVersion{
		Name:          write.Name,
		Version:       write.Version,
		Type:          write.Type,
		Prompt:        write.Prompt,
		Config:        write.Config,
		CommitMessage: write.CommitMessage,
		Labels:        write.Labels,
		CreatedAt:     write.CreatedAt,
	}))
}

func renderPrompt(prompt *store.PromptVersion) promptResponse {
	return promptResponse{
		Name:          prompt.Name,
		Version:       prompt.Version,
		Type:          prompt.Type,
		Prompt:        json.RawMessage(prompt.Prompt),
		Config:        json.RawMessage(prompt.Config),
		CommitMessage: prompt.CommitMessage,
		Labels:        orEmpty(prompt.Labels),
		CreatedAt:     formatTime(prompt.CreatedAt),
	}
}

// validate turns a version request into a write job. The body's own shape
// decides text-versus-chat; a stated `type` must agree with it, and whether an
// unstated one may be adopted from earlier versions is settled in the write
// transaction, where the stored type is visible (Decision 20, 2026-08-27).
func (in *promptVersionRequest) validate(projectID, name string) (*store.PromptVersionWrite, error) {
	if !jsonValue(in.Prompt) {
		return nil, fmt.Errorf(`"prompt" is required`)
	}
	shape, err := promptShape(in.Prompt)
	if err != nil {
		return nil, err
	}
	if in.Type != nil {
		switch *in.Type {
		case store.PromptText, store.PromptChat:
		default:
			return nil, fmt.Errorf(`"type" must be %q or %q, got %q`,
				store.PromptText, store.PromptChat, *in.Type)
		}
		if *in.Type != shape {
			return nil, fmt.Errorf(`a %s prompt's "prompt" must be %s, and this one is %s`,
				*in.Type, promptShapeDescription(*in.Type), promptShapeDescription(shape))
		}
	}

	if in.ExpectVersion != nil && *in.ExpectVersion < 0 {
		return nil, fmt.Errorf(`"expect_version" must be 0 or a positive whole number`)
	}

	write := &store.PromptVersionWrite{
		ProjectID:     projectID,
		Name:          name,
		Type:          shape,
		TypeStated:    in.Type != nil,
		ExpectVersion: in.ExpectVersion,
		Prompt:        compactJSON(in.Prompt),
		CommitMessage: in.CommitMessage,
		CreatedAt:     time.Now().UnixNano(),
	}
	if jsonValue(in.Config) {
		var object map[string]any
		if err := json.Unmarshal(in.Config, &object); err != nil {
			return nil, fmt.Errorf(`"config" must be a JSON object`)
		}
		write.Config = compactJSON(in.Config)
	}
	for _, label := range in.Labels {
		if err := validLabel(label); err != nil {
			return nil, err
		}
	}
	write.Labels = in.Labels
	return write, nil
}

// promptShape reads text-versus-chat off the body itself: a string is a text
// prompt, an array of messages is a chat one. Nothing is interpreted beyond
// that — the body is stored and returned verbatim (#15).
func promptShape(raw json.RawMessage) (string, error) {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return store.PromptText, nil
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return "", fmt.Errorf(`"prompt" must be a string (a text prompt) or an array of {role, content} messages (a chat prompt)`)
	}
	if len(messages) == 0 {
		return "", fmt.Errorf(`a chat prompt needs at least one message`)
	}
	for i, message := range messages {
		var role string
		if err := json.Unmarshal(message["role"], &role); len(message["role"]) == 0 || err != nil || role == "" {
			return "", fmt.Errorf(`message %d needs a non-empty "role"`, i)
		}
		content, present := message["content"]
		if !present || !jsonValue(content) {
			return "", fmt.Errorf(`message %d needs a "content"`, i)
		}
		// Versions are append-only, so an empty content cannot be
		// edited away later; the client that reads it back would only
		// find out when the model call fails (#23).
		var asText string
		if err := json.Unmarshal(content, &asText); err == nil && asText == "" {
			return "", fmt.Errorf(`message %d has an empty "content"`, i)
		}
	}
	return store.PromptChat, nil
}

func promptShapeDescription(shape string) string {
	if shape == store.PromptChat {
		return "an array of messages"
	}
	return "a string"
}

// handleGetPrompt resolves ?version=N xor ?label=L, or the latest version when
// neither is given (#13). There is no implicit `production` default: a
// convention the server invents is a support question factory.
func (s *Server) handleGetPrompt(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	name, ok := promptName(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "version", "label")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rawVersion, rawLabel := values.Get("version"), values.Get("label")
	if rawVersion != "" && rawLabel != "" {
		writeError(w, http.StatusBadRequest, "a prompt is fetched by version or by label, not both")
		return
	}

	// The selector and the message for when it resolves to nothing are
	// decided together: they answer the same question, and deriving the
	// second from the raw query a second time is how they drift apart.
	var (
		selector store.PromptSelector
		missing  = fmt.Sprintf("prompt %q not found", name)
	)
	switch {
	case rawVersion != "":
		version, err := strconv.Atoi(rawVersion)
		if err != nil || version < 1 {
			writeError(w, http.StatusBadRequest, "version must be a positive whole number")
			return
		}
		selector.Version = version
		missing = fmt.Sprintf("prompt %q has no version %d", name, version)
	case rawLabel != "" && rawLabel != store.LatestLabel:
		// `latest` is not a stored label; it falls through to the
		// unqualified path, which is the same query (#11).
		if err := validName("label", rawLabel); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		selector.Label = rawLabel
		missing = fmt.Sprintf("prompt %q has no label %q", name, rawLabel)
	}

	prompt, err := s.store.Prompt(project.ID, name, selector)
	if err != nil {
		slog.Error("read prompt failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the prompt")
		return
	}
	if prompt == nil {
		writeError(w, http.StatusNotFound, missing)
		return
	}
	w.Header().Set("Cache-Control", promptCacheControl)
	writeJSON(w, http.StatusOK, renderPrompt(prompt))
}

// handleListPromptVersions lists a name's versions newest first, without the
// bodies: version lists are for picking and diffing (#18).
func (s *Server) handleListPromptVersions(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	name, ok := promptName(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "limit", "cursor", "direction")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := pageSize(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	backward, err := pageDirection(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	after := 0
	cursor := values.Get("cursor")
	if cursor != "" {
		parts, err := decodeCursor(cursor, 1)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if after, err = strconv.Atoi(parts[0]); err != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
	}

	versions, labels, err := s.store.PromptVersions(project.ID, name, limit+1, after, backward)
	if err != nil {
		slog.Error("list prompt versions failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list the versions")
		return
	}
	// A name exists only by having versions, so an empty first page means
	// the name is unknown. An empty later page is just the end of a walk.
	if len(versions) == 0 && cursor == "" {
		writeError(w, http.StatusNotFound, fmt.Sprintf("prompt %q not found", name))
		return
	}

	versions, prev, next := trimPage(versions, limit, backward, cursor,
		func(v store.PromptVersionSummary) string { return encodeCursor(strconv.Itoa(v.Version)) })
	out := make([]promptVersionSummaryResponse, 0, len(versions))
	for _, version := range versions {
		out = append(out, promptVersionSummaryResponse{
			Version:       version.Version,
			CommitMessage: version.CommitMessage,
			Labels:        orEmpty(version.Labels),
			CreatedAt:     formatTime(version.CreatedAt),
		})
	}
	if labels == nil {
		labels = map[string]int{}
	}
	w.Header().Set("Cache-Control", promptCacheControl)
	writeJSON(w, http.StatusOK, promptVersionListResponse{
		Versions: out, Labels: labels, NextCursor: next, PrevCursor: prev})
}

// handleListPrompts lists names alphabetically with where their labels point.
// `updated_at` is when the newest version of the name was created.
func (s *Server) handleListPrompts(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "limit", "cursor", "direction")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := pageSize(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	backward, err := pageDirection(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	after := ""
	cursor := values.Get("cursor")
	if cursor != "" {
		parts, err := decodeCursor(cursor, 1)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		after = parts[0]
	}

	prompts, err := s.store.Prompts(project.ID, limit+1, after, backward)
	if err != nil {
		slog.Error("list prompts failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list prompts")
		return
	}

	prompts, prev, next := trimPage(prompts, limit, backward, cursor,
		func(p store.PromptSummary) string { return encodeCursor(p.Name) })
	out := make([]promptSummaryResponse, 0, len(prompts))
	for _, prompt := range prompts {
		labels := prompt.Labels
		if labels == nil {
			labels = map[string]int{}
		}
		out = append(out, promptSummaryResponse{
			Name:          prompt.Name,
			Type:          prompt.Type,
			LatestVersion: prompt.LatestVersion,
			Labels:        labels,
			UpdatedAt:     formatTime(prompt.UpdatedAt),
		})
	}
	w.Header().Set("Cache-Control", promptCacheControl)
	writeJSON(w, http.StatusOK, promptListResponse{Prompts: out, NextCursor: next, PrevCursor: prev})
}

// promptDeletionNote is why a trace that ran this prompt is untouched by the
// delete. It is part of the dry run because "what happens to the traces" is
// the question a person asks before typing the name back (spec 021 #7).
const promptDeletionNote = "traces that ran this prompt keep the name and version they recorded; " +
	"the trace filter goes on answering for it"

// handleDeletePrompt removes a name whole — every version and every label —
// following the dry-run/confirm contract of spec 005 #8 (spec 021 #7). Full
// management includes removing a prompt that was a mistake or a
// rename-by-recreation, and the store had no write for it.
func (s *Server) handleDeletePrompt(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	name, ok := promptName(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "confirm")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	confirm := values.Get("confirm")
	if confirm == "" {
		counts, err := s.store.PromptPreview(project.ID, name)
		if err != nil {
			slog.Error("prompt preview failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read what the prompt holds")
			return
		}
		// A name exists only by having versions, the same rule the version
		// listing reads a 404 off.
		if counts.Versions == 0 {
			writeError(w, http.StatusNotFound, fmt.Sprintf("prompt %q not found", name))
			return
		}
		writeJSON(w, http.StatusOK, renderPromptDeletion(true, name, counts).
			put("confirm", name).
			put("note", promptDeletionNote))
		return
	}

	deletion := &store.PromptDelete{ProjectID: project.ID, Name: name, Confirm: confirm}
	if !s.submit(w, r, deletion) {
		return
	}
	writeJSON(w, http.StatusOK, renderPromptDeletion(false, name, deletion.Counts).
		put("deleted", true))
}

func renderPromptDeletion(dryRun bool, name string, counts store.PromptCounts) object {
	return object{}.
		put("dry_run", dryRun).
		put("name", name).
		put("would_delete", object{}.
			put("versions", counts.Versions).
			put("labels", counts.Labels))
}

// handlePutPromptLabel points a label at a version, creating or moving it.
// This is the deploy path: promote by moving `production` forward, roll back
// by moving it back — no redeploy and no new version (#12).
func (s *Server) handlePutPromptLabel(w http.ResponseWriter, r *http.Request) {
	project, name, label, ok := s.labelTarget(w, r)
	if !ok {
		return
	}
	var request struct {
		Version *int `json:"version"`
	}
	if !s.readJSON(w, r, &request) {
		return
	}
	if request.Version == nil || *request.Version < 1 {
		writeError(w, http.StatusBadRequest, `"version" must be a positive whole number`)
		return
	}

	write := &store.PromptLabelWrite{
		ProjectID: project.ID,
		Name:      name,
		Label:     label,
		Version:   *request.Version,
	}
	if !s.submit(w, r, write) {
		return
	}
	writeJSON(w, http.StatusOK, labelResponse{Label: label, Version: write.Version})
}

// handleDeletePromptLabel removes a label, answering with the version it
// pointed at.
func (s *Server) handleDeletePromptLabel(w http.ResponseWriter, r *http.Request) {
	project, name, label, ok := s.labelTarget(w, r)
	if !ok {
		return
	}
	write := &store.PromptLabelWrite{
		ProjectID: project.ID,
		Name:      name,
		Label:     label,
		Remove:    true,
	}
	if !s.submit(w, r, write) {
		return
	}
	writeJSON(w, http.StatusOK, labelResponse{Label: label, Version: write.Removed})
}

// handlePromptDiff serves a unified text diff between two versions of a name
// (#21). "What changed in the prompt between yesterday's run and today's" is
// an agent question, and a patch is the answer format every consumer already
// reads.
func (s *Server) handlePromptDiff(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	name, ok := promptName(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "from", "to")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var versions [2]int
	for i, side := range []string{"from", "to"} {
		raw := values.Get(side)
		if raw == "" {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("%s is required: a diff needs two versions", side))
			return
		}
		version, err := strconv.Atoi(raw)
		if err != nil || version < 1 {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("%s must be a positive whole number, got %q", side, raw))
			return
		}
		versions[i] = version
	}

	var loaded [2]*store.PromptVersion
	for i, version := range versions {
		prompt, err := s.store.Prompt(project.ID, name, store.PromptSelector{Version: version})
		if err != nil {
			slog.Error("read prompt failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read the prompt")
			return
		}
		if prompt == nil {
			writeError(w, http.StatusNotFound,
				fmt.Sprintf("prompt %q has no version %d", name, version))
			return
		}
		loaded[i] = prompt
	}

	fromLabel := fmt.Sprintf("version %d", versions[0])
	toLabel := fmt.Sprintf("version %d", versions[1])
	// Both halves of a version are diffed: a run can change because the
	// text changed or because the model parameters did, and a diff that
	// showed only one of them would answer "nothing changed" to a real
	// change.
	diff := unifiedDiff("prompt", fromLabel, toLabel, pretty(loaded[0].Prompt), pretty(loaded[1].Prompt)) +
		unifiedDiff("config", fromLabel, toLabel, pretty(loaded[0].Config), pretty(loaded[1].Config))

	w.Header().Set("Cache-Control", promptCacheControl)
	writeJSON(w, http.StatusOK, object{}.
		put("name", name).
		put("from", versions[0]).
		put("to", versions[1]).
		put("diff", diff))
}

// pretty renders stored JSON as indented text, which is what makes a line diff
// of it meaningful: the compact form stores a whole prompt on one line, and a
// one-line diff says only that the line changed.
func pretty(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		// Stored JSON always indents; if that stopped being true, the
		// bytes as stored are the honest thing to diff.
		return string(raw)
	}
	return out.String() + "\n"
}

// labelTarget authenticates and validates the {name}/{label} pair both label
// handlers start from.
func (s *Server) labelTarget(w http.ResponseWriter, r *http.Request) (*store.Project, string, string, bool) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return nil, "", "", false
	}
	name, ok := promptName(w, r)
	if !ok {
		return nil, "", "", false
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, "", "", false
	}
	label := r.PathValue("label")
	if err := validLabel(label); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, "", "", false
	}
	return project, name, label, true
}

// promptName validates the {name} path segment.
func promptName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if err := validName("prompt name", name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	return name, true
}

// validLabel checks a label name and keeps `latest` out of storage: it is
// always the highest version, computed at read time (#11).
func validLabel(label string) error {
	if label == store.LatestLabel {
		return fmt.Errorf("%q is reserved: it always names the highest version", store.LatestLabel)
	}
	return validName("label", label)
}

func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
