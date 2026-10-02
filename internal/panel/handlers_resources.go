package panel

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/uio-o/smart-gateway/internal/store"
)

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		items, err := s.store.Services(ctx)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"services": items})
		return nil

	case http.MethodPost:
		var in store.Service
		if err := decodeJSON(r, &in); err != nil {
			return badRequest("%v", err)
		}
		in.ID = ""
		saved, err := s.store.SaveService(ctx, in)
		if err != nil {
			return classifyStoreError(err)
		}
		writeJSON(w, http.StatusCreated, saved)
		return nil

	default:
		return &httpError{Status: http.StatusMethodNotAllowed, Msg: "method not allowed"}
	}
}

func (s *Server) handleServiceItem(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id, ok := pathID(r.URL.Path, "/api/services/")
	if !ok {
		return &httpError{Status: http.StatusNotFound, Msg: "not found"}
	}

	switch r.Method {
	case http.MethodGet:
		item, err := s.store.Service(ctx, id)
		if err != nil {
			return classifyStoreError(err)
		}
		writeJSON(w, http.StatusOK, item)
		return nil

	case http.MethodPut:
		existing, err := s.store.Service(ctx, id)
		if err != nil {
			return classifyStoreError(err)
		}
		var in store.Service
		if err := decodeJSON(r, &in); err != nil {
			return badRequest("%v", err)
		}
		in.ID = existing.ID
		in.CreatedAt = existing.CreatedAt
		saved, err := s.store.SaveService(ctx, in)
		if err != nil {
			return classifyStoreError(err)
		}
		writeJSON(w, http.StatusOK, saved)
		return nil

	case http.MethodDelete:
		if err := s.store.DeleteService(ctx, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return classifyStoreError(err)
			}
			// The store refuses to delete a service a route depends on.
			return conflict("%v", err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
		return nil

	default:
		return &httpError{Status: http.StatusMethodNotAllowed, Msg: "method not allowed"}
	}
}

// --- routes ---

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		items, err := s.store.Routes(ctx)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"routes": items})
		return nil

	case http.MethodPost:
		var in store.Route
		if err := decodeJSON(r, &in); err != nil {
			return badRequest("%v", err)
		}
		in.ID = ""
		// Assign a relay port automatically when the operator did not pick one.
		if in.Port == 0 && len(in.NodeNames) > 0 {
			port, err := s.store.AllocatePort(ctx)
			if err != nil {
				return conflict("%v", err)
			}
			in.Port = port
		}
		saved, err := s.store.SaveRoute(ctx, in)
		if err != nil {
			return classifyStoreError(err)
		}
		writeJSON(w, http.StatusCreated, saved)
		return nil

	default:
		return &httpError{Status: http.StatusMethodNotAllowed, Msg: "method not allowed"}
	}
}

func (s *Server) handleRouteItem(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	// The order sub-resource lets the UI reorder a chain of hops.
	if strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/order") {
		return &httpError{Status: http.StatusNotFound, Msg: "not found"}
	}

	id, ok := pathID(r.URL.Path, "/api/routes/")
	if !ok {
		return &httpError{Status: http.StatusNotFound, Msg: "not found"}
	}

	switch r.Method {
	case http.MethodGet:
		item, err := s.store.Route(ctx, id)
		if err != nil {
			return classifyStoreError(err)
		}
		writeJSON(w, http.StatusOK, item)
		return nil

	case http.MethodPut:
		existing, err := s.store.Route(ctx, id)
		if err != nil {
			return classifyStoreError(err)
		}
		var in store.Route
		if err := decodeJSON(r, &in); err != nil {
			return badRequest("%v", err)
		}
		in.ID = existing.ID
		in.CreatedAt = existing.CreatedAt
		if in.Port == 0 {
			in.Port = existing.Port
		}
		if in.Port == 0 && len(in.NodeNames) > 0 {
			port, err := s.store.AllocatePort(ctx)
			if err != nil {
				return conflict("%v", err)
			}
			in.Port = port
		}
		saved, err := s.store.SaveRoute(ctx, in)
		if err != nil {
			return classifyStoreError(err)
		}
		writeJSON(w, http.StatusOK, saved)
		return nil

	case http.MethodDelete:
		if err := s.deleteRouteGuarded(ctx, id); err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
		return nil

	default:
		return &httpError{Status: http.StatusMethodNotAllowed, Msg: "method not allowed"}
	}
}

// deleteRouteGuarded refuses to remove a route the scheduling policy names.
func (s *Server) deleteRouteGuarded(ctx context.Context, id string) error {
	rt, err := s.store.Route(ctx, id)
	if err != nil {
		return classifyStoreError(err)
	}
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return err
	}
	switch settings.Routing.Mode {
	case "static":
		if settings.Routing.Static == rt.Name {
			return conflict("route %q is the active static route; change the routing policy first", rt.Name)
		}
	case "primary_backup":
		if settings.Routing.Primary == rt.Name || settings.Routing.Backup == rt.Name {
			return conflict("route %q is referenced by the routing policy; change it first", rt.Name)
		}
	}
	if err := s.store.DeleteRoute(ctx, id); err != nil {
		return classifyStoreError(err)
	}
	return nil
}

// --- tokens ---

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		items, err := s.store.Tokens(ctx)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, map[string]any{"tokens": items})
		return nil

	case http.MethodPost:
		var in store.Token
		if err := decodeJSON(r, &in); err != nil {
			return badRequest("%v", err)
		}
		in.ID = ""
		saved, err := s.store.SaveToken(ctx, in)
		if err != nil {
			return classifyStoreError(err)
		}
		writeJSON(w, http.StatusCreated, saved)
		return nil

	default:
		return &httpError{Status: http.StatusMethodNotAllowed, Msg: "method not allowed"}
	}
}

func (s *Server) handleTokenItem(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id, ok := pathID(r.URL.Path, "/api/tokens/")
	if !ok {
		return &httpError{Status: http.StatusNotFound, Msg: "not found"}
	}
	if r.Method != http.MethodDelete {
		return &httpError{Status: http.StatusMethodNotAllowed, Msg: "method not allowed"}
	}
	if err := s.store.DeleteToken(ctx, id); err != nil {
		return classifyStoreError(err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
	return nil
}

// --- settings ---

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		item, err := s.store.Settings(ctx)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, item)
		return nil

	case http.MethodPut:
		var in store.Settings
		if err := decodeJSON(r, &in); err != nil {
			return badRequest("%v", err)
		}
		saved, err := s.store.SaveSettings(ctx, in)
		if err != nil {
			return badRequest("%v", err)
		}
		writeJSON(w, http.StatusOK, saved)
		return nil

	default:
		return &httpError{Status: http.StatusMethodNotAllowed, Msg: "method not allowed"}
	}
}
