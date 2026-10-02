package panel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/uio-o/smart-gateway/internal/store"
)

// maxBodyBytes bounds panel request bodies; these are small configuration
// documents, never large payloads.
const maxBodyBytes = 1 << 20

// decodeJSON reads a JSON request body with a size limit and unknown-field
// rejection, so a typo in a client is reported rather than silently ignored.
func decodeJSON(r *http.Request, dst any) error {
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("request body is empty")
		}
		return err
	}
	// Reject trailing content so a truncated document cannot pass as valid.
	if dec.More() {
		return errors.New("request body contains more than one JSON value")
	}
	return nil
}

// writeJSON renders a response.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status is already written; nothing further can be reported.
		return
	}
}

// writeError renders an error response.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

// pathID extracts the trailing identifier from a collection path.
func pathID(path, prefix string) (string, bool) {
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.Trim(rest, "/")
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	id := decodePathSegment(rest)
	if id == "" {
		return "", false
	}
	return id, true
}

// decodePathSegment reverses the URL escaping applied to identifiers.
func decodePathSegment(seg string) string {
	seg = strings.ReplaceAll(seg, "%2F", "/")
	seg = strings.ReplaceAll(seg, "%2f", "/")
	return seg
}

// --- nodes ---

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		nodes, err := s.store.Nodes(ctx)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
		return nil

	case http.MethodPost:
		var in store.Node
		if err := decodeJSON(r, &in); err != nil {
			return badRequest("%v", err)
		}
		in.ID = ""
		saved, err := s.store.SaveNode(ctx, in)
		if err != nil {
			return classifyStoreError(err)
		}
		writeJSON(w, http.StatusCreated, saved)
		return nil

	default:
		return &httpError{Status: http.StatusMethodNotAllowed, Msg: "method not allowed"}
	}
}

func (s *Server) handleNodeItem(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id, ok := pathID(r.URL.Path, "/api/nodes/")
	if !ok {
		return &httpError{Status: http.StatusNotFound, Msg: "not found"}
	}

	switch r.Method {
	case http.MethodGet:
		n, err := s.store.Node(ctx, id)
		if err != nil {
			return classifyStoreError(err)
		}
		writeJSON(w, http.StatusOK, n)
		return nil

	case http.MethodPut:
		existing, err := s.store.Node(ctx, id)
		if err != nil {
			return classifyStoreError(err)
		}
		var in store.Node
		if err := decodeJSON(r, &in); err != nil {
			return badRequest("%v", err)
		}
		in.ID = existing.ID
		in.CreatedAt = existing.CreatedAt
		saved, err := s.store.SaveNode(ctx, in)
		if err != nil {
			return classifyStoreError(err)
		}
		writeJSON(w, http.StatusOK, saved)
		return nil

	case http.MethodDelete:
		if err := s.deleteNodeGuarded(ctx, id); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
		return nil

	default:
		return &httpError{Status: http.StatusMethodNotAllowed, Msg: "method not allowed"}
	}
}

// deleteNodeGuarded refuses to remove a node that routes still depend on.
//
// Deleting it silently would make the entry configuration unrenderable, so the
// operator is told which routes to change first.
func (s *Server) deleteNodeGuarded(ctx context.Context, id string) error {
	node, err := s.store.Node(ctx, id)
	if err != nil {
		return classifyStoreError(err)
	}
	routes, err := s.store.Routes(ctx)
	if err != nil {
		return err
	}
	var dependents []string
	for _, r := range routes {
		for _, name := range r.NodeNames {
			if name == node.Name {
				dependents = append(dependents, r.Name)
				break
			}
		}
	}
	if len(dependents) > 0 {
		return conflict("node %q is still used by route(s): %s", node.Name, strings.Join(dependents, ", "))
	}
	if err := s.store.DeleteNode(ctx, id); err != nil {
		return classifyStoreError(err)
	}
	return nil
}

// classifyStoreError maps store errors onto HTTP status codes.
func classifyStoreError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return &httpError{Status: http.StatusNotFound, Msg: "not found"}
	}
	if strings.Contains(err.Error(), "already exists") {
		return &httpError{Status: http.StatusConflict, Msg: err.Error()}
	}
	return err
}
