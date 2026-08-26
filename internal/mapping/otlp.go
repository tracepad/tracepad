package mapping

import (
	"errors"
	"fmt"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// The OTLP export envelope, hand-coded against the wire format instead of
// imported from go.opentelemetry.io/proto/otlp/collector/trace/v1
// (spec 002 Decision 18, 2026-08-26): that package declares the gRPC
// TraceService, so importing it drags google.golang.org/grpc and
// grpc-gateway into a binary that speaks OTLP over plain HTTP only. Both
// messages are one field deep:
//
//	ExportTraceServiceRequest  { repeated ResourceSpans resource_spans = 1 }
//	ExportTraceServiceResponse { ExportTracePartialSuccess partial_success = 1 }
//	ExportTracePartialSuccess  { int64 rejected_spans = 1; string error_message = 2 }
//
// The generated ResourceSpans type does the actual decoding, so nothing here
// interprets span contents.
const (
	fieldResourceSpans  = 1
	fieldPartialSuccess = 1
	fieldRejectedSpans  = 1
	fieldErrorMessage   = 2
)

// ErrMalformedBody reports a body that is not a decodable
// ExportTraceServiceRequest.
var ErrMalformedBody = errors.New("malformed OTLP request body")

// DecodeExportRequest unpacks an ExportTraceServiceRequest body into its
// ResourceSpans. Unknown fields are skipped, as protobuf requires, so a
// newer exporter that adds a field still ingests.
func DecodeExportRequest(body []byte) ([]*tracepb.ResourceSpans, error) {
	var out []*tracepb.ResourceSpans
	for len(body) > 0 {
		num, typ, n := protowire.ConsumeTag(body)
		if n < 0 {
			return nil, fmt.Errorf("%w: %v", ErrMalformedBody, protowire.ParseError(n))
		}
		body = body[n:]

		if num == fieldResourceSpans && typ == protowire.BytesType {
			raw, n := protowire.ConsumeBytes(body)
			if n < 0 {
				return nil, fmt.Errorf("%w: %v", ErrMalformedBody, protowire.ParseError(n))
			}
			body = body[n:]
			rs := &tracepb.ResourceSpans{}
			if err := proto.Unmarshal(raw, rs); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrMalformedBody, err)
			}
			out = append(out, rs)
			continue
		}

		n = protowire.ConsumeFieldValue(num, typ, body)
		if n < 0 {
			return nil, fmt.Errorf("%w: %v", ErrMalformedBody, protowire.ParseError(n))
		}
		body = body[n:]
	}
	return out, nil
}

// EncodeExportResponse renders an ExportTraceServiceResponse. A fully
// accepted export answers with an empty message; rejected spans are reported
// through partial_success, which every OTLP exporter already understands
// (spec 002 #13).
func EncodeExportResponse(rejectedSpans int64, errorMessage string) []byte {
	if rejectedSpans == 0 && errorMessage == "" {
		return nil
	}
	var partial []byte
	if rejectedSpans != 0 {
		partial = protowire.AppendTag(partial, fieldRejectedSpans, protowire.VarintType)
		partial = protowire.AppendVarint(partial, uint64(rejectedSpans))
	}
	if errorMessage != "" {
		partial = protowire.AppendTag(partial, fieldErrorMessage, protowire.BytesType)
		partial = protowire.AppendString(partial, errorMessage)
	}
	var out []byte
	out = protowire.AppendTag(out, fieldPartialSuccess, protowire.BytesType)
	out = protowire.AppendBytes(out, partial)
	return out
}

// EncodeExportRequest is the inverse of DecodeExportRequest. It exists for
// tests and fixture generation; the server never encodes a request.
func EncodeExportRequest(resourceSpans []*tracepb.ResourceSpans) ([]byte, error) {
	var out []byte
	for _, rs := range resourceSpans {
		raw, err := proto.Marshal(rs)
		if err != nil {
			return nil, err
		}
		out = protowire.AppendTag(out, fieldResourceSpans, protowire.BytesType)
		out = protowire.AppendBytes(out, raw)
	}
	return out, nil
}
