package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MCP over Streamable HTTP (JSON responses, stateless). Every registry
// operation the caller's token can use becomes a tool; tool names replace the
// dots with underscores (projects.create -> projects_create).

var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const mcpInstructions = `IvaraBase is a self-hosted Postgres platform. Each project is its own database + role.
Start with system_health and projects_list. Use db_schema before writing SQL or migrations.
Prefer db_migrations_apply for schema changes (transactional, recorded, idempotent).
Before risky changes run backups_create for the project.
Destructive tools return confirmation_required with a preview first; repeat with confirm=true.
If the result is approval_required, tell the user to approve it in the IvaraBase UI, then poll approvals_get.
Errors carry a code and a hint - read the hint before retrying.`

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func mcpToolName(op string) string { return strings.ReplaceAll(op, ".", "_") }

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		w.WriteHeader(200)
		return
	case http.MethodGet:
		// no server-initiated stream
		w.Header().Set("Allow", "POST, DELETE")
		w.WriteHeader(405)
		return
	default:
		w.WriteHeader(405)
		return
	}
	p, err := s.principal(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ivarabase"`)
		writeError(w, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		w.WriteHeader(400)
		return
	}
	trim := strings.TrimSpace(string(body))
	if strings.HasPrefix(trim, "[") {
		var batch []rpcReq
		if err := json.Unmarshal(body, &batch); err != nil {
			writeJSON(w, 400, rpcResponse(nil, nil, &rpcErr{-32700, "parse error"}))
			return
		}
		var out []any
		for _, req := range batch {
			if resp := s.mcpDispatch(r, p, req); resp != nil {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(202)
			return
		}
		writeJSON(w, 200, out)
		return
	}
	var req rpcReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 400, rpcResponse(nil, nil, &rpcErr{-32700, "parse error"}))
		return
	}
	resp := s.mcpDispatch(r, p, req)
	if resp == nil {
		w.WriteHeader(202)
		return
	}
	writeJSON(w, 200, resp)
}

func rpcResponse(id json.RawMessage, result any, e *rpcErr) map[string]any {
	m := map[string]any{"jsonrpc": "2.0"}
	if id != nil {
		m["id"] = id
	} else {
		m["id"] = nil
	}
	if e != nil {
		m["error"] = e
	} else {
		m["result"] = result
	}
	return m
}

func (s *Server) mcpDispatch(r *http.Request, p *Principal, req rpcReq) map[string]any {
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &params)
		ver := mcpVersions[0]
		for _, v := range mcpVersions {
			if v == params.ProtocolVersion {
				ver = v
			}
		}
		return rpcResponse(req.ID, map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "ivarabase", "title": "IvaraBase", "version": Version, "websiteUrl": RepoURL},
			"instructions":    mcpInstructions,
		}, nil)
	case "ping":
		return rpcResponse(req.ID, map[string]any{}, nil)
	case "tools/list":
		var tools []any
		for _, o := range s.app.Ops.All() {
			if o.Level > p.Level {
				continue
			}
			desc := o.Summary
			if o.Description != "" {
				desc += ". " + o.Description
			}
			desc += fmt.Sprintf(" [needs %s access]", o.Level)
			if o.Destructive {
				desc += " DESTRUCTIVE: returns a preview unless confirm=true."
			} else if o.DestructiveIf != nil {
				desc += " Some arguments make it destructive: then it returns a preview unless confirm=true."
			}
			tools = append(tools, map[string]any{
				"name": mcpToolName(o.Name), "title": o.Summary, "description": desc,
				"inputSchema": o.InputSchema(),
				"annotations": map[string]any{
					"readOnlyHint":    o.Level == LevelRead && o.DestructiveIf == nil,
					"destructiveHint": o.Destructive || o.DestructiveIf != nil,
					"openWorldHint":   false,
				},
			})
		}
		return rpcResponse(req.ID, map[string]any{"tools": tools}, nil)
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return rpcResponse(req.ID, nil, &rpcErr{-32602, "invalid params"})
		}
		opName := ""
		for _, o := range s.app.Ops.All() {
			if mcpToolName(o.Name) == params.Name || o.Name == params.Name {
				opName = o.Name
			}
		}
		if opName == "" {
			return rpcResponse(req.ID, nil, &rpcErr{-32602, "unknown tool " + params.Name})
		}
		res, err := s.app.Ops.Execute(r.Context(), p, opName, Args(params.Arguments))
		if err != nil {
			var ap *ApprovalPending
			if errors.As(err, &ap) {
				return rpcResponse(req.ID, toolText(map[string]any{
					"status": "approval_required", "approval_id": ap.ID, "preview": ap.Preview,
					"next": "ask the user to approve it in the IvaraBase UI (Approvals page), then call approvals_get with this approval_id",
				}, false), nil)
			}
			return rpcResponse(req.ID, toolText(map[string]any{"error": AsAPIError(err)}, true), nil)
		}
		return rpcResponse(req.ID, toolText(res, false), nil)
	case "resources/list":
		return rpcResponse(req.ID, map[string]any{"resources": []any{}}, nil)
	case "prompts/list":
		return rpcResponse(req.ID, map[string]any{"prompts": []any{}}, nil)
	}
	if isNotification {
		return nil
	}
	return rpcResponse(req.ID, nil, &rpcErr{-32601, "method not found: " + req.Method})
}

func toolText(v any, isErr bool) map[string]any {
	b, _ := json.MarshalIndent(v, "", "  ")
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}, "isError": isErr}
}
