package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// Score configs (spec 014 #15–#17): a config pins what a score's name means,
// and every `POST /api/v1/scores` naming a configured name is checked against
// it inside the write. Declarative: PUT replaces the whole config, DELETE
// removes it, and neither touches a stored score.

// scoreConfigRequest is the body of PUT /api/v1/score-configs/{name}.
type scoreConfigRequest struct {
	DataType    string   `json:"data_type"`
	Direction   *string  `json:"direction"`
	Min         *float64 `json:"min"`
	Max         *float64 `json:"max"`
	Categories  []string `json:"categories"`
	Description string   `json:"description"`
}

type scoreConfigResponse struct {
	Name        string   `json:"name"`
	DataType    string   `json:"data_type"`
	Direction   *string  `json:"direction"`
	Min         *float64 `json:"min"`
	Max         *float64 `json:"max"`
	Categories  []string `json:"categories"`
	Description *string  `json:"description"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

type scoreConfigListResponse struct {
	Configs []scoreConfigResponse `json:"configs"`
}

type scoreConfigDeletedResponse struct {
	Name string `json:"name"`
}

// handlePutScoreConfig creates or replaces a config (#17). A body equal to
// the stored one is a no-op, so a harness that declares its configs at the
// top of every run leaves no trail behind.
func (s *Server) handlePutScoreConfig(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.scoreConfigTarget(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request scoreConfigRequest
	if !s.readJSON(w, r, &request) {
		return
	}
	config, err := request.validate(name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	put := &store.ScoreConfigPut{ProjectID: project.ID, Config: config, Now: time.Now().UnixNano()}
	if !s.submit(w, r, put) {
		return
	}
	writeJSON(w, http.StatusOK, renderScoreConfig(put.Stored))
}

// validate applies #16 and the API contract: the direction is required where
// a sign means something and forbidden where it does not, bounds belong to
// numeric names only, and a categorical name needs its categories.
func (in *scoreConfigRequest) validate(name string) (*store.ScoreConfig, error) {
	if !validDataType(in.DataType) {
		return nil, fmt.Errorf(`"data_type" must be one of numeric, boolean, categorical, text, got %q`, in.DataType)
	}
	config := &store.ScoreConfig{Name: name, DataType: in.DataType, Description: in.Description}

	signed := in.DataType == store.ScoreNumeric || in.DataType == store.ScoreBoolean
	switch {
	case signed && in.Direction == nil:
		return nil, fmt.Errorf(`a %s config needs a "direction": higher, lower or none`, in.DataType)
	case !signed && in.Direction != nil:
		return nil, fmt.Errorf(`a %s config takes no "direction": its values are not an axis`, in.DataType)
	case in.Direction != nil:
		switch *in.Direction {
		case store.DirectionHigher, store.DirectionLower, store.DirectionNone:
			config.Direction = *in.Direction
		default:
			return nil, fmt.Errorf(`"direction" must be higher, lower or none, got %q`, *in.Direction)
		}
	}

	if in.Min != nil || in.Max != nil {
		if in.DataType != store.ScoreNumeric {
			return nil, fmt.Errorf(`"min" and "max" belong to a numeric config, not a %s one`, in.DataType)
		}
		if in.Min != nil && in.Max != nil && *in.Min > *in.Max {
			return nil, fmt.Errorf(`"min" %v is above "max" %v`, *in.Min, *in.Max)
		}
		config.Min, config.Max = in.Min, in.Max
	}

	if in.DataType == store.ScoreCategorical {
		if len(in.Categories) == 0 {
			return nil, fmt.Errorf(`a categorical config needs a non-empty "categories" list`)
		}
		for i, category := range in.Categories {
			if category == "" {
				return nil, fmt.Errorf(`"categories" has an empty entry at index %d`, i)
			}
			if slices.Index(in.Categories, category) != i {
				return nil, fmt.Errorf(`"categories" lists %q twice`, category)
			}
		}
		config.Categories = in.Categories
	} else if in.Categories != nil {
		return nil, fmt.Errorf(`"categories" belongs to a categorical config, not a %s one`, in.DataType)
	}
	return config, nil
}

// handleListScoreConfigs lists a project's configs by name, whole.
func (s *Server) handleListScoreConfigs(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	configs, err := s.store.ScoreConfigs(project.ID)
	if err != nil {
		slog.Error("list score configs failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list the score configs")
		return
	}
	out := make([]scoreConfigResponse, 0, len(configs))
	for _, config := range configs {
		out = append(out, renderScoreConfig(config))
	}
	writeJSON(w, http.StatusOK, scoreConfigListResponse{Configs: out})
}

// handleGetScoreConfig serves one config by name.
func (s *Server) handleGetScoreConfig(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.scoreConfigTarget(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	config, err := s.store.ScoreConfig(project.ID, name)
	if err != nil {
		slog.Error("read score config failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the score config")
		return
	}
	if config == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("score config %q not found", name))
		return
	}
	writeJSON(w, http.StatusOK, renderScoreConfig(config))
}

// handleDeleteScoreConfig removes a config. The scores it admitted stand: a
// config is a rule for what comes next (#15, #20).
func (s *Server) handleDeleteScoreConfig(w http.ResponseWriter, r *http.Request) {
	project, name, ok := s.scoreConfigTarget(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.submit(w, r, &store.ScoreConfigDelete{ProjectID: project.ID, Name: name}) {
		return
	}
	writeJSON(w, http.StatusOK, scoreConfigDeletedResponse{Name: name})
}

// scoreConfigTarget authenticates and validates the {name}. A config's name
// is a score's name, so it has the score name's bounds and no grammar beyond
// them: any name a score may carry can have a config.
func (s *Server) scoreConfigTarget(w http.ResponseWriter, r *http.Request) (*store.Project, string, bool) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return nil, "", false
	}
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "score config name must not be empty")
		return nil, "", false
	}
	if len(name) > maxScoreNameLength {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("score config name must be at most %d characters", maxScoreNameLength))
		return nil, "", false
	}
	return project, name, true
}

func renderScoreConfig(config *store.ScoreConfig) scoreConfigResponse {
	return scoreConfigResponse{
		Name:        config.Name,
		DataType:    config.DataType,
		Direction:   nullable(config.Direction),
		Min:         config.Min,
		Max:         config.Max,
		Categories:  config.Categories,
		Description: nullable(config.Description),
		CreatedAt:   formatTime(config.CreatedAt),
		UpdatedAt:   formatTime(config.UpdatedAt),
	}
}
