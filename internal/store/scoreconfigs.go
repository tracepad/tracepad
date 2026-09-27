package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Score configs (spec 014 #15–#17): a config pins what a score's name means
// — its type, the direction that counts as better, a range or a category
// list — and a score whose name has one must satisfy it. The binding is by
// name and nothing else: a config a client had to opt into by id would be
// drift with extra steps. Declarative, no versions: a PUT replaces the whole
// row and governs what is accepted from then on; stored scores are never
// re-validated.

// Score directions (#16).
const (
	DirectionHigher = "higher"
	DirectionLower  = "lower"
	DirectionNone   = "none"
)

// ScoreConfig is one name's contract.
type ScoreConfig struct {
	Name     string
	DataType string
	// Direction is set for numeric and boolean names and empty for the
	// rest (#16).
	Direction string
	// Min and Max bound a numeric name's value; nil is unbounded.
	Min *float64
	Max *float64
	// Categories is the closed set a categorical name's string_value must
	// belong to; nil for every other type.
	Categories  []string
	Description string
	CreatedAt   int64
	UpdatedAt   int64
}

// Same reports whether two configs say the same thing, which is what makes a
// re-PUT of the same body a no-op (#17).
func (c *ScoreConfig) Same(other *ScoreConfig) bool {
	return c.DataType == other.DataType && c.Direction == other.Direction &&
		floatEqual(c.Min, other.Min) && floatEqual(c.Max, other.Max) &&
		slices.Equal(c.Categories, other.Categories) && c.Description == other.Description
}

func floatEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// check is Decision 15 applied to one score: the type must be the config's,
// a numeric value must lie inside the bounds, a categorical value must be one
// of the categories. The message names the rule, and the caller names the
// item.
func (c *ScoreConfig) check(score *Score) error {
	if score.DataType != c.DataType {
		return fmt.Errorf("%q is %s in its config, got %s", score.Name, c.DataType, score.DataType)
	}
	switch c.DataType {
	case ScoreNumeric:
		if score.Value == nil {
			return nil
		}
		if c.Min != nil && *score.Value < *c.Min {
			return fmt.Errorf("value %v is below the config's min %v", *score.Value, *c.Min)
		}
		if c.Max != nil && *score.Value > *c.Max {
			return fmt.Errorf("value %v is above the config's max %v", *score.Value, *c.Max)
		}
	case ScoreCategorical:
		if score.StringValue != nil && !slices.Contains(c.Categories, *score.StringValue) {
			return fmt.Errorf("%q is not among the config's categories (%s)",
				*score.StringValue, strings.Join(c.Categories, ", "))
		}
	}
	return nil
}

// ScoreConfigPut is PUT /api/v1/score-configs/{name}: create or replace the
// whole config (#17). A body equal to the stored one writes nothing, so the
// harness that declares its configs at the top of every run leaves no trail
// of updates behind.
type ScoreConfigPut struct {
	ProjectID string
	Config    *ScoreConfig
	Now       int64

	// Stored is the row after the write, filled by apply.
	Stored *ScoreConfig
}

func (p *ScoreConfigPut) apply(tx *sql.Tx) error {
	p.Stored = nil
	existing, err := scoreConfigByName(tx, p.ProjectID, p.Config.Name)
	if err != nil {
		return err
	}
	if existing != nil && existing.Same(p.Config) {
		p.Stored = existing
		return nil
	}
	categories, err := encodeCategories(p.Config.Categories)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO score_configs (project_id, name, data_type, direction, min_value, max_value,
		                            categories, description, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, name) DO UPDATE SET
		   data_type   = excluded.data_type,
		   direction   = excluded.direction,
		   min_value   = excluded.min_value,
		   max_value   = excluded.max_value,
		   categories  = excluded.categories,
		   description = excluded.description,
		   updated_at  = excluded.updated_at`,
		p.ProjectID, p.Config.Name, p.Config.DataType, nullString(p.Config.Direction),
		nullFloat(p.Config.Min), nullFloat(p.Config.Max), categories,
		nullString(p.Config.Description), p.Now, p.Now,
	); err != nil {
		return fmt.Errorf("put score config %s: %w", p.Config.Name, err)
	}
	p.Stored, err = scoreConfigByName(tx, p.ProjectID, p.Config.Name)
	return err
}

// ScoreConfigDelete is DELETE /api/v1/score-configs/{name}: the rule goes,
// the scores it admitted stay (#17, #20).
type ScoreConfigDelete struct {
	ProjectID string
	Name      string
}

func (d *ScoreConfigDelete) apply(tx *sql.Tx) error {
	result, err := tx.Exec(`DELETE FROM score_configs WHERE project_id = ? AND name = ?`,
		d.ProjectID, d.Name)
	if err != nil {
		return fmt.Errorf("delete score config %s: %w", d.Name, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return &Rejection{Kind: RejectNotFound, Message: fmt.Sprintf("score config %q not found", d.Name)}
	}
	return nil
}

// ScoreConfigs lists a project's configs by name, whole: a project declares a
// handful of names, and a config is read by nobody but its author.
func (s *Store) ScoreConfigs(ctx context.Context, projectID string) ([]*ScoreConfig, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+scoreConfigColumns+` FROM score_configs WHERE project_id = ? ORDER BY name`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list score configs: %w", err)
	}
	defer rows.Close()

	var out []*ScoreConfig
	for rows.Next() {
		config, err := scanScoreConfig(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, config)
	}
	return out, rows.Err()
}

// ScoreConfig returns one config, or nil when the name has none.
func (s *Store) ScoreConfig(ctx context.Context, projectID, name string) (*ScoreConfig, error) {
	config, err := scanScoreConfig(s.db.QueryRowContext(ctx,
		`SELECT `+scoreConfigColumns+` FROM score_configs WHERE project_id = ? AND name = ?`,
		projectID, name))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return config, err
}

const scoreConfigColumns = `name, data_type, direction, min_value, max_value, categories, description, created_at, updated_at`

func scoreConfigByName(tx *sql.Tx, projectID, name string) (*ScoreConfig, error) {
	config, err := scanScoreConfig(tx.QueryRow(
		`SELECT `+scoreConfigColumns+` FROM score_configs WHERE project_id = ? AND name = ?`,
		projectID, name))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return config, err
}

func scanScoreConfig(row scanner) (*ScoreConfig, error) {
	var (
		config      ScoreConfig
		direction   sql.NullString
		minValue    sql.NullFloat64
		maxValue    sql.NullFloat64
		categories  sql.NullString
		description sql.NullString
	)
	if err := row.Scan(&config.Name, &config.DataType, &direction, &minValue, &maxValue,
		&categories, &description, &config.CreatedAt, &config.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, err
		}
		return nil, fmt.Errorf("scan score config: %w", err)
	}
	config.Direction, config.Description = direction.String, description.String
	if minValue.Valid {
		config.Min = &minValue.Float64
	}
	if maxValue.Valid {
		config.Max = &maxValue.Float64
	}
	if categories.Valid {
		if err := json.Unmarshal([]byte(categories.String), &config.Categories); err != nil {
			return nil, fmt.Errorf("decode categories of score config %s: %w", config.Name, err)
		}
	}
	return &config, nil
}

func encodeCategories(categories []string) (any, error) {
	if len(categories) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(categories)
	if err != nil {
		return nil, fmt.Errorf("encode categories: %w", err)
	}
	return string(raw), nil
}
