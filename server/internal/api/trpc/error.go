package trpc

import (
	"net/http"
)

// Code is the string key of a tRPC error code ("BAD_REQUEST", "NOT_FOUND",
// ...). On the wire an error carries both this key (inside error data, as
// data.code) and a numeric JSON-RPC-style code (error.code); see Number and
// HTTPStatus.
//
// The tables below mirror tRPC 11.18.0 exactly:
//   - numeric codes: packages/server/src/unstable-core-do-not-import/rpc/codes.ts
//     (TRPC_ERROR_CODES_BY_KEY, "copying the last digits of HTTP 4XX errors")
//   - HTTP statuses: packages/server/src/unstable-core-do-not-import/http/getHTTPStatusCode.ts
//     (JSONRPC2_TO_HTTP_CODE)
type Code string

const (
	CodeParseError           Code = "PARSE_ERROR"
	CodeBadRequest           Code = "BAD_REQUEST"
	CodeInternalServerError  Code = "INTERNAL_SERVER_ERROR"
	CodeNotImplemented       Code = "NOT_IMPLEMENTED"
	CodeBadGateway           Code = "BAD_GATEWAY"
	CodeServiceUnavailable   Code = "SERVICE_UNAVAILABLE"
	CodeGatewayTimeout       Code = "GATEWAY_TIMEOUT"
	CodeUnauthorized         Code = "UNAUTHORIZED"
	CodePaymentRequired      Code = "PAYMENT_REQUIRED"
	CodeForbidden            Code = "FORBIDDEN"
	CodeNotFound             Code = "NOT_FOUND"
	CodeMethodNotSupported   Code = "METHOD_NOT_SUPPORTED"
	CodeTimeout              Code = "TIMEOUT"
	CodeConflict             Code = "CONFLICT"
	CodePreconditionFailed   Code = "PRECONDITION_FAILED"
	CodePayloadTooLarge      Code = "PAYLOAD_TOO_LARGE"
	CodeUnsupportedMediaType Code = "UNSUPPORTED_MEDIA_TYPE"
	CodeUnprocessableContent Code = "UNPROCESSABLE_CONTENT"
	CodePreconditionRequired Code = "PRECONDITION_REQUIRED"
	CodeTooManyRequests      Code = "TOO_MANY_REQUESTS"
	CodeClientClosedRequest  Code = "CLIENT_CLOSED_REQUEST"
)

// codeNumbers mirrors TRPC_ERROR_CODES_BY_KEY (rpc/codes.ts:11-44). Note that
// INTERNAL_SERVER_ERROR, NOT_IMPLEMENTED, BAD_GATEWAY, SERVICE_UNAVAILABLE and
// GATEWAY_TIMEOUT all share -32603, exactly as upstream.
var codeNumbers = map[Code]int{
	CodeParseError:           -32700,
	CodeBadRequest:           -32600,
	CodeInternalServerError:  -32603,
	CodeNotImplemented:       -32603,
	CodeBadGateway:           -32603,
	CodeServiceUnavailable:   -32603,
	CodeGatewayTimeout:       -32603,
	CodeUnauthorized:         -32001,
	CodePaymentRequired:      -32002,
	CodeForbidden:            -32003,
	CodeNotFound:             -32004,
	CodeMethodNotSupported:   -32005,
	CodeTimeout:              -32008,
	CodeConflict:             -32009,
	CodePreconditionFailed:   -32012,
	CodePayloadTooLarge:      -32013,
	CodeUnsupportedMediaType: -32015,
	CodeUnprocessableContent: -32022,
	CodePreconditionRequired: -32028,
	CodeTooManyRequests:      -32029,
	CodeClientClosedRequest:  -32099,
}

// codeStatuses mirrors JSONRPC2_TO_HTTP_CODE (getHTTPStatusCode.ts:7-32).
var codeStatuses = map[Code]int{
	CodeParseError:           http.StatusBadRequest,
	CodeBadRequest:           http.StatusBadRequest,
	CodeUnauthorized:         http.StatusUnauthorized,
	CodePaymentRequired:      http.StatusPaymentRequired,
	CodeForbidden:            http.StatusForbidden,
	CodeNotFound:             http.StatusNotFound,
	CodeMethodNotSupported:   http.StatusMethodNotAllowed,
	CodeTimeout:              http.StatusRequestTimeout,
	CodeConflict:             http.StatusConflict,
	CodePreconditionFailed:   http.StatusPreconditionFailed,
	CodePayloadTooLarge:      http.StatusRequestEntityTooLarge,
	CodeUnsupportedMediaType: http.StatusUnsupportedMediaType,
	CodeUnprocessableContent: http.StatusUnprocessableEntity,
	CodePreconditionRequired: http.StatusPreconditionRequired,
	CodeTooManyRequests:      http.StatusTooManyRequests,
	CodeClientClosedRequest:  499, // nginx-style "client closed request"
	CodeInternalServerError:  http.StatusInternalServerError,
	CodeNotImplemented:       http.StatusNotImplemented,
	CodeBadGateway:           http.StatusBadGateway,
	CodeServiceUnavailable:   http.StatusServiceUnavailable,
	CodeGatewayTimeout:       http.StatusGatewayTimeout,
}

// Number returns the numeric JSON-RPC-style code emitted as error.code.
// Unknown codes fall back to INTERNAL_SERVER_ERROR's -32603, matching
// getStatusCodeFromKey's fallback behavior for statuses.
func (c Code) Number() int {
	if n, ok := codeNumbers[c]; ok {
		return n
	}
	return codeNumbers[CodeInternalServerError]
}

// HTTPStatus returns the HTTP status tRPC 11 pairs with this error code
// (getStatusCodeFromKey, getHTTPStatusCode.ts:59-63). Unknown codes map to 500.
func (c Code) HTTPStatus() int {
	if s, ok := codeStatuses[c]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// Error is a tRPC procedure error: the Go equivalent of throwing a TRPCError.
// On the wire it becomes {"error":{"json":{"message","code","data":{...}}}}
// (the whole shape is superjson-wrapped; see superjson.go).
type Error struct {
	Code    Code
	Message string
	// Data is merged into the wire error.data next to the standard code,
	// httpStatus and path entries. Upstream's errorFormatter (packages/api/src/trpc.ts:84-92)
	// always adds zodError, error and isBoardLayoutBoundsError there; homarr-go
	// handlers can set them manually when a consumer needs them.
	Data map[string]any
}

// NewError builds a tRPC error.
func NewError(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Error implements the error interface so *Error can wrap Go errors and flow
// through regular error handling.
func (e *Error) Error() string {
	return string(e.Code) + ": " + e.Message
}

// HTTPStatus returns the HTTP status tRPC 11 pairs with the error code.
func (e *Error) HTTPStatus() int {
	return e.Code.HTTPStatus()
}

// Internal converts an unexpected Go error into a tRPC INTERNAL_SERVER_ERROR
// (the equivalent of getTRPCErrorFromUnknown wrapping an unknown cause).
func Internal(err error) *Error {
	return &Error{
		Code:    CodeInternalServerError,
		Message: err.Error(),
	}
}

// shape builds the untransformed error envelope payload — the getErrorShape
// output of tRPC 11 (error/getErrorShape.ts:21-35): message, numeric code and
// data{code, httpStatus, path?}. Upstream adds data.stack only in dev mode, so
// homarr-go never emits it. path is omitted when empty, like upstream.
func (e *Error) shape(path string) errorShape {
	data := map[string]any{
		"code":       string(e.Code),
		"httpStatus": e.HTTPStatus(),
	}
	if path != "" {
		data["path"] = path
	}
	for k, v := range e.Data {
		if k == "code" || k == "httpStatus" || k == "path" {
			continue // never let custom data shadow the standard entries
		}
		data[k] = v
	}
	return errorShape{
		Message: e.Message,
		Code:    e.Code.Number(),
		Data:    data,
	}
}
