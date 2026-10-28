package app

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
)

func NewServiceHandler(t *Tideglass) http.Handler {
	return NewServiceHandlerWithToken(t, os.Getenv("TIDEGLASS_SERVICE_TOKEN"))
}

func NewServiceHandlerWithToken(t *Tideglass, token string) http.Handler {
	token = strings.TrimSpace(token)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !authorizedHTTPHost(r) {
			writeHTTPError(w, http.StatusForbidden, "forbidden host")
			return
		}
		writeHTTPJSON(w, http.StatusOK, map[string]any{"status": "ok", "schema": schemaVersion})
	})
	mux.HandleFunc("/resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !authorizedHTTPHost(r) {
			writeHTTPError(w, http.StatusForbidden, "forbidden host")
			return
		}
		if !authorizedServiceRequest(r, token) {
			writeHTTPError(w, http.StatusUnauthorized, "missing or invalid service token")
			return
		}
		if !authorizedHTTPWrite(r) {
			writeHTTPError(w, http.StatusForbidden, "forbidden write")
			return
		}
		defer r.Body.Close()
		var request IntentRequestEnvelope
		body := http.MaxBytesReader(w, r.Body, 1<<20)
		dec := json.NewDecoder(body)
		dec.UseNumber()
		dec.DisallowUnknownFields()
		if err := dec.Decode(&request); err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeHTTPError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			writeHTTPError(w, http.StatusBadRequest, err.Error())
			return
		}
		var trailing any
		if err := dec.Decode(&trailing); err != io.EOF {
			writeHTTPError(w, http.StatusBadRequest, "request body must contain exactly one JSON object")
			return
		}
		if err := authorizeHTTPResolveRequest(request); err != nil {
			writeHTTPError(w, http.StatusForbidden, err.Error())
			return
		}
		response, err := t.ResolveIntent(r.Context(), ResolveOptions{Request: request})
		if err != nil {
			writeHTTPError(w, statusForResolveError(err), err.Error())
			return
		}
		writeHTTPJSON(w, http.StatusOK, response)
	})
	mux.HandleFunc("/resource", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !authorizedHTTPHost(r) {
			writeHTTPError(w, http.StatusForbidden, "forbidden host")
			return
		}
		if !authorizedServiceRequest(r, token) {
			writeHTTPError(w, http.StatusUnauthorized, "missing or invalid service token")
			return
		}
		uri := r.URL.Query().Get("uri")
		response, err := t.ResolveIntent(r.Context(), ResolveOptions{Request: IntentRequestEnvelope{URI: uri}, NoPersist: true})
		if err != nil {
			writeHTTPError(w, statusForResolveError(err), err.Error())
			return
		}
		writeHTTPJSON(w, http.StatusOK, response)
	})
	return mux
}

func HandleMCPOnce(ctx context.Context, t *Tideglass, in io.Reader, out io.Writer) error {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}
	const maxMCPInputBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(in, maxMCPInputBytes+1))
	if err != nil {
		return writeMCPError(out, nil, -32700, err.Error())
	}
	if len(data) > maxMCPInputBytes {
		return writeMCPError(out, nil, -32700, "mcp input too large")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		return writeMCPError(out, nil, -32700, err.Error())
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return writeMCPError(out, nil, -32700, "mcp input must contain exactly one JSON-RPC request")
	}
	if req.JSONRPC != "2.0" {
		return writeMCPError(out, req.ID, -32600, "invalid JSON-RPC version")
	}
	var result any
	switch req.Method {
	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		if err := decodeJSONUseNumber(req.Params, &params); err != nil {
			return writeMCPError(out, req.ID, -32602, err.Error())
		}
		response, err := t.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{URI: params.URI}, NoPersist: true})
		if err != nil {
			return writeMCPError(out, req.ID, -32000, err.Error())
		}
		text, err := json.MarshalIndent(response, "", "  ")
		if err != nil {
			return writeMCPError(out, req.ID, -32603, err.Error())
		}
		result = map[string]any{"contents": []map[string]any{{"uri": params.URI, "mimeType": "application/json", "text": string(text)}}}
	case "tools/call":
		var params struct {
			Name      string                `json:"name"`
			Arguments IntentRequestEnvelope `json:"arguments"`
		}
		if err := decodeJSONUseNumber(req.Params, &params); err != nil {
			return writeMCPError(out, req.ID, -32602, err.Error())
		}
		if params.Name != "tideglass.resolve_intent" {
			return writeMCPError(out, req.ID, -32601, fmt.Sprintf("unsupported mcp tool %q", params.Name))
		}
		if err := authorizeMCPResolveRequest(params.Arguments); err != nil {
			return writeMCPError(out, req.ID, -32000, err.Error())
		}
		response, err := t.ResolveIntent(ctx, ResolveOptions{Request: params.Arguments})
		if err != nil {
			return writeMCPError(out, req.ID, -32000, err.Error())
		}
		result = map[string]any{"content": []map[string]any{{"type": "text", "text": mustJSONText(response)}}}
	default:
		return writeMCPError(out, req.ID, -32601, fmt.Sprintf("unsupported mcp method %q", req.Method))
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func writeMCPError(out io.Writer, id any, code int, message string) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func decodeJSONUseNumber(data []byte, value any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	return dec.Decode(value)
}

func writeHTTPJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(value)
}

func writeHTTPError(w http.ResponseWriter, status int, message string) {
	writeHTTPJSON(w, status, map[string]any{"error": message})
}

func authorizedHTTPHost(r *http.Request) bool {
	host := r.Host
	if strings.Contains(host, ":") {
		if parsedHost, _, err := net.SplitHostPort(host); err == nil {
			host = parsedHost
		}
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func authorizedHTTPWrite(r *http.Request) bool {
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if contentType != "application/json" {
		return false
	}
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin != "" && !strings.HasPrefix(origin, "http://127.0.0.1") && !strings.HasPrefix(origin, "http://localhost") && !strings.HasPrefix(origin, "http://[::1]") {
		return false
	}
	return true
}

func authorizedServiceRequest(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Tideglass-Token")), []byte(token)) == 1 {
		return true
	}
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	return strings.HasPrefix(auth, prefix) && subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, prefix)), []byte(token)) == 1
}

func authorizeHTTPResolveRequest(request IntentRequestEnvelope) error {
	if request.Disclosure.AllowSensitive {
		return errors.New("http resolve cannot enable sensitive disclosure without a capability token")
	}
	if request.Freshness.RequireReviewed != nil && !*request.Freshness.RequireReviewed {
		return errors.New("http resolve cannot disable reviewed-claim freshness")
	}
	return nil
}

func authorizeMCPResolveRequest(request IntentRequestEnvelope) error {
	if request.Disclosure.AllowSensitive {
		return errors.New("mcp resolve cannot enable sensitive disclosure without trusted server capability")
	}
	if request.Freshness.RequireReviewed != nil && !*request.Freshness.RequireReviewed {
		return errors.New("mcp resolve cannot disable reviewed-claim freshness without trusted server capability")
	}
	return nil
}

func statusForResolveError(err error) int {
	if isRequestError(err) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func isRequestError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "intent request uri is required") ||
		strings.Contains(message, "unsupported intent uri") ||
		strings.Contains(message, "unsupported request schema_version") ||
		strings.Contains(message, "unsupported disclosure mode") ||
		strings.Contains(message, "unsupported task mode") ||
		strings.Contains(message, "unsupported autonomy") ||
		strings.Contains(message, "unsupported proof.hash") ||
		strings.Contains(message, "unsupported proof.commitments") ||
		strings.Contains(message, "proof.zk_predicates are not supported") ||
		strings.Contains(message, "purpose must match task.goal") ||
		strings.Contains(message, "invalid freshness.max_age")
}

func mustJSONText(value any) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(data)
}
