package mapping

import "encoding/json"

// The debug encoding is the golden-fixture format (spec 002, Testing #1): a
// stable, human-reviewable rendering of a mapped export. It is a test oracle,
// not an API — Go sorts map keys when marshalling, and empty fields are
// omitted, so a golden file diff shows exactly what a rule change did.

type debugResult struct {
	Dialect      string             `json:"dialect"`
	Skipped      int64              `json:"skipped,omitempty"`
	SkipReason   string             `json:"skip_reason,omitempty"`
	Traces       []debugTrace       `json:"traces"`
	Observations []debugObservation `json:"observations"`
}

type debugTrace struct {
	ID          string         `json:"id"`
	Name        string         `json:"name,omitempty"`
	UserID      string         `json:"user_id,omitempty"`
	SessionID   string         `json:"session_id,omitempty"`
	Environment string         `json:"environment,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

type debugObservation struct {
	TraceID             string         `json:"trace_id"`
	ID                  string         `json:"id"`
	ParentObservationID string         `json:"parent_observation_id,omitempty"`
	Type                string         `json:"type"`
	Name                string         `json:"name,omitempty"`
	StartTime           int64          `json:"start_time"`
	EndTime             int64          `json:"end_time"`
	Model               string         `json:"model,omitempty"`
	ModelParameters     map[string]any `json:"model_parameters,omitempty"`
	Level               string         `json:"level"`
	StatusMessage       string         `json:"status_message,omitempty"`
	Usage               map[string]any `json:"usage,omitempty"`
	CostDetails         map[string]any `json:"cost_details,omitempty"`
	ProvidedCost        bool           `json:"provided_cost,omitempty"`
	Input               any            `json:"input,omitempty"`
	Output              any            `json:"output,omitempty"`
	Metadata            map[string]any `json:"metadata,omitempty"`
}

// DebugJSON renders the mapped export in the golden-fixture format.
func (r *Result) DebugJSON() ([]byte, error) {
	out := debugResult{
		Dialect:      r.Dialect,
		Skipped:      r.Skipped,
		SkipReason:   r.SkipReason,
		Traces:       make([]debugTrace, 0, len(r.Traces)),
		Observations: make([]debugObservation, 0, len(r.Observations)),
	}
	for _, t := range r.Traces {
		out.Traces = append(out.Traces, debugTrace{
			ID:          t.ID,
			Name:        t.Name,
			UserID:      t.UserID,
			SessionID:   t.SessionID,
			Environment: t.Environment,
			Tags:        t.Tags,
			Metadata:    t.Metadata,
		})
	}
	for _, o := range r.Observations {
		out.Observations = append(out.Observations, debugObservation{
			TraceID:             o.TraceID,
			ID:                  o.ID,
			ParentObservationID: o.ParentObservationID,
			Type:                o.Type,
			Name:                o.Name,
			StartTime:           o.StartTime,
			EndTime:             o.EndTime,
			Model:               o.Model,
			ModelParameters:     o.ModelParameters,
			Level:               o.Level,
			StatusMessage:       o.StatusMessage,
			Usage:               o.Usage,
			CostDetails:         o.CostDetails,
			ProvidedCost:        o.ProvidedCost(),
			Input:               o.Input,
			Output:              o.Output,
			Metadata:            o.Metadata,
		})
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}
