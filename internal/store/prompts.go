package store

import (
	"database/sql"
	"fmt"
)

// Versioned prompts (spec 003). Versions are an append-only audit trail per
// (project, name): nothing is edited in place, and a rollback is a label move
// rather than a rewrite (#10, #12). The version number is assigned inside the
// write transaction, which is what makes the sequence gapless without a lock
// of its own — the writer is already the only goroutine committing (#9).

// Prompt types (#10). The set is closed by a CHECK in schema 0003.
const (
	PromptText = "text"
	PromptChat = "chat"
)

// LatestLabel is the reserved, virtual label: it always resolves to the
// highest version and is never stored, so `?label=latest` and an unqualified
// GET cannot disagree (#11).
const LatestLabel = "latest"

// PromptVersion is one immutable version of a named prompt.
type PromptVersion struct {
	Name    string
	Version int
	Type    string
	// Prompt is raw JSON: a string for a text prompt, an array of
	// {role, content} objects for a chat one. Stored and returned
	// verbatim — interpolation is the client's job (#15).
	Prompt        []byte
	Config        []byte
	CommitMessage string
	Labels        []string
	CreatedAt     int64
}

// PromptVersionSummary is one row of a version list: everything but the
// bodies, which come from the single-prompt GET (#18).
type PromptVersionSummary struct {
	Version       int
	CommitMessage string
	Labels        []string
	CreatedAt     int64
}

// PromptSummary is one row of the prompt list: a name, its shape, where its
// labels point, and when it last gained a version.
type PromptSummary struct {
	Name          string
	Type          string
	LatestVersion int
	Labels        map[string]int
	UpdatedAt     int64
}

// PromptSelector picks one version of a name: an explicit version, a label,
// or — with both unset — the highest version (#13).
type PromptSelector struct {
	Version int
	Label   string
}

// PromptVersionWrite appends a version to a name (#10).
type PromptVersionWrite struct {
	ProjectID string
	Name      string
	// Type is the shape of Prompt, which the handler reads off the body
	// itself; TypeStated records whether the client also declared it.
	// Only the first version of a name is required to declare it (#10),
	// and only the stored versions can say whether this is the first, so
	// that check happens here rather than in the handler, where it would
	// race (Decision 20, 2026-08-27).
	Type          string
	TypeStated    bool
	Prompt        []byte
	Config        []byte
	CommitMessage string
	Labels        []string
	CreatedAt     int64
	// Version is the number this commit assigned, filled in by apply so
	// the handler can answer with the version it created.
	Version int
}

func (p *PromptVersionWrite) apply(tx *sql.Tx) error {
	var (
		existingType string
		latest       int
	)
	err := tx.QueryRow(
		`SELECT type, version FROM prompts WHERE project_id = ? AND name = ?
		 ORDER BY version DESC LIMIT 1`, p.ProjectID, p.Name).Scan(&existingType, &latest)
	switch {
	case err == sql.ErrNoRows:
		if !p.TypeStated {
			return &Rejection{
				Kind:    RejectInvalid,
				Message: fmt.Sprintf(`prompt %q does not exist yet: its first version must state a "type"`, p.Name),
			}
		}
	case err != nil:
		return fmt.Errorf("read prompt %s: %w", p.Name, err)
	case p.Type != existingType:
		// A name that changes shape between versions breaks every
		// client fetching it by label, so the type is part of the
		// name's contract (#10).
		return &Rejection{
			Kind: RejectInvalid,
			Message: fmt.Sprintf("prompt %q is a %s prompt; this version is a %s prompt",
				p.Name, existingType, p.Type),
		}
	}

	version := latest + 1
	if _, err := tx.Exec(
		`INSERT INTO prompts (project_id, name, version, type, prompt, config, commit_message, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ProjectID, p.Name, version, p.Type, string(p.Prompt), nullJSON(p.Config),
		nullString(p.CommitMessage), p.CreatedAt); err != nil {
		return fmt.Errorf("insert prompt %s version %d: %w", p.Name, version, err)
	}
	for _, label := range p.Labels {
		if err := setPromptLabel(tx, p.ProjectID, p.Name, label, version); err != nil {
			return err
		}
	}
	p.Version = version
	return nil
}

// PromptLabelWrite creates, moves or removes one label (#12). Moving a label
// is the deploy path — promote by pointing `production` at a new version, roll
// back by pointing it at the old one — so it is a single-row write, not a new
// version.
type PromptLabelWrite struct {
	ProjectID string
	Name      string
	Label     string
	// Version is the version to point at; ignored when Remove is set.
	Version int
	Remove  bool
	// Removed is the version the label pointed at, filled in by a removing
	// commit so the handler can report what it took away.
	Removed int
}

func (p *PromptLabelWrite) apply(tx *sql.Tx) error {
	if p.Remove {
		var version int
		err := tx.QueryRow(
			`SELECT version FROM prompt_labels WHERE project_id = ? AND name = ? AND label = ?`,
			p.ProjectID, p.Name, p.Label).Scan(&version)
		if err == sql.ErrNoRows {
			return &Rejection{
				Kind:    RejectNotFound,
				Message: fmt.Sprintf("prompt %q has no label %q", p.Name, p.Label),
			}
		}
		if err != nil {
			return fmt.Errorf("read label %s of prompt %s: %w", p.Label, p.Name, err)
		}
		if _, err := tx.Exec(
			`DELETE FROM prompt_labels WHERE project_id = ? AND name = ? AND label = ?`,
			p.ProjectID, p.Name, p.Label); err != nil {
			return fmt.Errorf("delete label %s of prompt %s: %w", p.Label, p.Name, err)
		}
		p.Removed = version
		return nil
	}

	var exists int
	err := tx.QueryRow(`SELECT 1 FROM prompts WHERE project_id = ? AND name = ? AND version = ?`,
		p.ProjectID, p.Name, p.Version).Scan(&exists)
	if err == sql.ErrNoRows {
		return &Rejection{
			Kind:    RejectNotFound,
			Message: fmt.Sprintf("prompt %q has no version %d", p.Name, p.Version),
		}
	}
	if err != nil {
		return fmt.Errorf("read prompt %s version %d: %w", p.Name, p.Version, err)
	}
	return setPromptLabel(tx, p.ProjectID, p.Name, p.Label, p.Version)
}

// setPromptLabel points a label at a version, moving it if it already exists.
// Uniqueness per (project, name, label) is what makes "which version is
// production" a single-row answer (#12).
func setPromptLabel(tx *sql.Tx, projectID, name, label string, version int) error {
	_, err := tx.Exec(
		`INSERT INTO prompt_labels (project_id, name, label, version) VALUES (?, ?, ?, ?)
		 ON CONFLICT(project_id, name, label) DO UPDATE SET version = excluded.version`,
		projectID, name, label, version)
	if err != nil {
		return fmt.Errorf("set label %s of prompt %s: %w", label, name, err)
	}
	return nil
}

// Prompt resolves one version of a name, or nil when the name, the version or
// the label does not exist.
func (s *Store) Prompt(projectID, name string, selector PromptSelector) (*PromptVersion, error) {
	version := selector.Version
	switch {
	case selector.Label != "":
		err := s.db.QueryRow(
			`SELECT version FROM prompt_labels WHERE project_id = ? AND name = ? AND label = ?`,
			projectID, name, selector.Label).Scan(&version)
		if err == sql.ErrNoRows {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("resolve label %s of prompt %s: %w", selector.Label, name, err)
		}
	case version == 0:
		// `latest` is computed, never stored (#11): the unqualified GET
		// and ?label=latest are this one query.
		var highest sql.NullInt64
		if err := s.db.QueryRow(`SELECT MAX(version) FROM prompts WHERE project_id = ? AND name = ?`,
			projectID, name).Scan(&highest); err != nil {
			return nil, fmt.Errorf("resolve latest version of prompt %s: %w", name, err)
		}
		if !highest.Valid {
			return nil, nil
		}
		version = int(highest.Int64)
	}

	var (
		prompt        PromptVersion
		config        sql.NullString
		commitMessage sql.NullString
		body          string
	)
	err := s.db.QueryRow(
		`SELECT type, prompt, config, commit_message, created_at
		 FROM prompts WHERE project_id = ? AND name = ? AND version = ?`,
		projectID, name, version).
		Scan(&prompt.Type, &body, &config, &commitMessage, &prompt.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read prompt %s version %d: %w", name, version, err)
	}
	prompt.Name, prompt.Version = name, version
	prompt.Prompt, prompt.CommitMessage = []byte(body), commitMessage.String
	if config.Valid {
		prompt.Config = []byte(config.String)
	}
	labels, err := s.promptLabelsByVersion(projectID, name)
	if err != nil {
		return nil, err
	}
	prompt.Labels = labels[version]
	return &prompt, nil
}

// PromptVersions lists a name's versions, newest first and without bodies
// (#18). afterVersion continues a previous page; zero starts at the newest.
func (s *Store) PromptVersions(projectID, name string, limit, afterVersion int) ([]PromptVersionSummary, error) {
	query := `SELECT version, commit_message, created_at FROM prompts
	          WHERE project_id = ? AND name = ?`
	args := []any{projectID, name}
	if afterVersion > 0 {
		query += ` AND version < ?`
		args = append(args, afterVersion)
	}
	query += ` ORDER BY version DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list versions of prompt %s: %w", name, err)
	}
	defer rows.Close()

	var out []PromptVersionSummary
	for rows.Next() {
		var (
			summary       PromptVersionSummary
			commitMessage sql.NullString
		)
		if err := rows.Scan(&summary.Version, &commitMessage, &summary.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan version of prompt %s: %w", name, err)
		}
		summary.CommitMessage = commitMessage.String
		out = append(out, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// One query for the name's labels, not one per version on the page: a
	// name has a handful of labels and a page has up to 500 versions.
	labels, err := s.promptLabelsByVersion(projectID, name)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Labels = labels[out[i].Version]
	}
	return out, nil
}

// Prompts lists names alphabetically. Ordering by name rather than by recency
// is what makes the cursor a keyset: a version written mid-walk cannot move a
// name to a page the client already read.
func (s *Store) Prompts(projectID string, limit int, afterName string) ([]PromptSummary, error) {
	// GROUP BY over the primary key's own order, rather than a correlated
	// MAX subquery evaluated once per candidate row. `type` and
	// `created_at` are bare columns beside MAX(version), which SQLite
	// defines as coming from the row that produced the maximum — exactly
	// the newest version's row, which is what a summary describes.
	query := `SELECT name, type, MAX(version), created_at FROM prompts
	          WHERE project_id = ?`
	args := []any{projectID}
	if afterName != "" {
		query += ` AND name > ?`
		args = append(args, afterName)
	}
	query += ` GROUP BY name ORDER BY name LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list prompts: %w", err)
	}
	defer rows.Close()

	var out []PromptSummary
	for rows.Next() {
		var summary PromptSummary
		if err := rows.Scan(&summary.Name, &summary.Type, &summary.LatestVersion, &summary.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan prompt: %w", err)
		}
		out = append(out, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	// The page is a contiguous run of names, so its labels come back in one
	// query bounded by its first and last name rather than one query per
	// name.
	labels, err := s.promptLabelsByName(projectID, out[0].Name, out[len(out)-1].Name)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if named := labels[out[i].Name]; named != nil {
			out[i].Labels = named
		}
	}
	return out, nil
}

// promptLabelsByVersion returns one name's labels grouped by the version each
// points at, alphabetically within a version. A name carries a handful of
// labels — `production`, `staging` — so reading them all at once is cheaper
// than asking per version.
func (s *Store) promptLabelsByVersion(projectID, name string) (map[int][]string, error) {
	rows, err := s.db.Query(
		`SELECT version, label FROM prompt_labels WHERE project_id = ? AND name = ?
		 ORDER BY label`, projectID, name)
	if err != nil {
		return nil, fmt.Errorf("read labels of prompt %s: %w", name, err)
	}
	defer rows.Close()

	out := map[int][]string{}
	for rows.Next() {
		var (
			version int
			label   string
		)
		if err := rows.Scan(&version, &label); err != nil {
			return nil, err
		}
		out[version] = append(out[version], label)
	}
	return out, rows.Err()
}

// promptLabelsByName returns the labels of every name in a range, each with
// the version it points at.
func (s *Store) promptLabelsByName(projectID, firstName, lastName string) (map[string]map[string]int, error) {
	rows, err := s.db.Query(
		`SELECT name, label, version FROM prompt_labels
		 WHERE project_id = ? AND name >= ? AND name <= ?`, projectID, firstName, lastName)
	if err != nil {
		return nil, fmt.Errorf("read labels of prompts %s..%s: %w", firstName, lastName, err)
	}
	defer rows.Close()

	out := map[string]map[string]int{}
	for rows.Next() {
		var (
			name    string
			label   string
			version int
		)
		if err := rows.Scan(&name, &label, &version); err != nil {
			return nil, err
		}
		if out[name] == nil {
			out[name] = map[string]int{}
		}
		out[name][label] = version
	}
	return out, rows.Err()
}
