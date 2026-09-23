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
//
// A ResourceSpans that does not decode is skipped and counted rather than
// failing the export: that is the whole point of decoding the envelope
// ourselves (Decision 18), and it is the same bargain as #13 one level up —
// one unusable part must not destroy the usable rest, which is still on disk
// in the raw body. Only an envelope we cannot walk at all is an error.
func DecodeExportRequest(body []byte) ([]*tracepb.ResourceSpans, int, error) {
	decoded, err := decodeExportProto(body)
	if err != nil {
		return nil, decoded.Unreadable, err
	}
	return decoded.ResourceSpans, decoded.Unreadable, nil
}

// decodeExportProto is DecodeExportRequest keeping, for each ResourceSpans it
// decoded, where its field sits in the body — which is what lets a rewrite of
// one be spliced back without re-encoding the rest (spec 041 #5).
func decodeExportProto(body []byte) (*ExportBody, error) {
	out := &ExportBody{source: body}
	offset := 0
	for offset < len(body) {
		start := offset
		num, typ, n := protowire.ConsumeTag(body[offset:])
		if n < 0 {
			return out, fmt.Errorf("%w: %v", ErrMalformedBody, protowire.ParseError(n))
		}
		offset += n

		if num == fieldResourceSpans && typ == protowire.BytesType {
			raw, n := protowire.ConsumeBytes(body[offset:])
			if n < 0 {
				return out, fmt.Errorf("%w: %v", ErrMalformedBody, protowire.ParseError(n))
			}
			offset += n
			rs := &tracepb.ResourceSpans{}
			if err := proto.Unmarshal(raw, rs); err != nil {
				out.Unreadable++
				continue
			}
			out.ResourceSpans = append(out.ResourceSpans, rs)
			out.spans = append(out.spans, [2]int{start, offset})
			continue
		}

		n = protowire.ConsumeFieldValue(num, typ, body[offset:])
		if n < 0 {
			return out, fmt.Errorf("%w: %v", ErrMalformedBody, protowire.ParseError(n))
		}
		offset += n
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

// DecodeExportResponse reads the partial success out of an
// ExportTraceServiceResponse. It exists for the export command (spec 019 #5),
// which is a *client* of somebody else's OTLP receiver and has to be able to
// say what that receiver reported: a 2xx carrying rejected spans means the
// bytes arrived and some of them were refused, which is the receiver's
// business to explain and ours to pass on rather than to retry.
//
// A body it cannot walk reports nothing rather than failing: the export
// succeeded, and a receiver whose response we cannot parse has still taken the
// batch.
func DecodeExportResponse(body []byte) (rejectedSpans int64, errorMessage string) {
	for len(body) > 0 {
		num, typ, n := protowire.ConsumeTag(body)
		if n < 0 {
			return rejectedSpans, errorMessage
		}
		body = body[n:]
		if num == fieldPartialSuccess && typ == protowire.BytesType {
			raw, n := protowire.ConsumeBytes(body)
			if n < 0 {
				return rejectedSpans, errorMessage
			}
			body = body[n:]
			return decodePartialSuccess(raw)
		}
		n = protowire.ConsumeFieldValue(num, typ, body)
		if n < 0 {
			return rejectedSpans, errorMessage
		}
		body = body[n:]
	}
	return rejectedSpans, errorMessage
}

func decodePartialSuccess(body []byte) (rejectedSpans int64, errorMessage string) {
	for len(body) > 0 {
		num, typ, n := protowire.ConsumeTag(body)
		if n < 0 {
			return rejectedSpans, errorMessage
		}
		body = body[n:]
		switch {
		case num == fieldRejectedSpans && typ == protowire.VarintType:
			value, n := protowire.ConsumeVarint(body)
			if n < 0 {
				return rejectedSpans, errorMessage
			}
			rejectedSpans, body = int64(value), body[n:]
		case num == fieldErrorMessage && typ == protowire.BytesType:
			value, n := protowire.ConsumeString(body)
			if n < 0 {
				return rejectedSpans, errorMessage
			}
			errorMessage, body = value, body[n:]
		default:
			n = protowire.ConsumeFieldValue(num, typ, body)
			if n < 0 {
				return rejectedSpans, errorMessage
			}
			body = body[n:]
		}
	}
	return rejectedSpans, errorMessage
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
