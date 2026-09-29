package mcpserver

import (
	"github.com/google/jsonschema-go/jsonschema"
)

// Schema helpers. Tight input schemas — enums for the closed sets, patterns
// for the hex ids, bounds on the page size — are part of the tool contract
// rather than decoration (#18): every constraint stated here is one class of
// call that cannot go wrong at runtime, and one fewer round trip spent
// discovering that it did.

func object(properties map[string]*jsonschema.Schema, required ...string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: properties, Required: required}
}

func text(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: description}
}

func matching(pattern, description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Pattern: pattern, Description: description}
}

func oneOf(description string, values ...string) *jsonschema.Schema {
	enum := make([]any, 0, len(values))
	for _, value := range values {
		enum = append(enum, value)
	}
	return &jsonschema.Schema{Type: "string", Enum: enum, Description: description}
}

func timestamp(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Format: "date-time", Description: description}
}

func bounded(min, max float64, description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "integer", Minimum: &min, Maximum: &max, Description: description}
}

func atLeast(min float64, description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "number", Minimum: &min, Description: description}
}

func list(items *jsonschema.Schema, description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", Items: items, Description: description}
}

func number(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "number", Description: description}
}

func integer(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "integer", Description: description}
}

func boolean(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "boolean", Description: description}
}

// orNull is a schema that also admits null: a field the API always sends,
// null when there is nothing to say.
func orNull(schema *jsonschema.Schema, description string) *jsonschema.Schema {
	schema.Types, schema.Type = []string{schema.Type, "null"}, ""
	schema.Description = description
	return schema
}

// anything is a value the API returns verbatim — whatever the client logged.
func anything(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Description: description}
}

// counters is an object whose keys are data — a categorical score's own values,
// which no schema can enumerate — and whose values are counts.
func counters(description string) *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:                 "object",
		AdditionalProperties: integer("How many scores carried this value."),
		Description:          description,
	}
}

// The two id shapes, stated once so a tool and the endpoint behind it cannot
// disagree about what an id looks like.
const (
	traceIDPattern       = `^[0-9a-f]{32}$`
	observationIDPattern = `^[0-9a-f]{16}$`
	namePattern          = `^[A-Za-z0-9][A-Za-z0-9._-]*$`
)

// Page bounds, the same the API enforces.
const (
	minLimit = 1
	maxLimit = 500
)
