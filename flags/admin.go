package flags

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ManavA/keel/httpx"
)

// Reader is the subset of [Store] the admin API needs. It is read-only, so
// an operator console cannot change rollout state through it.
type Reader interface {
	Get(ctx context.Context, key string) (Flag, error)
	List(ctx context.Context) ([]Flag, error)
}

// AdminAPI serves flag definitions for an operator console. Mount it behind
// whatever guards admin traffic, for example:
//
//	r := adminService.Router()
//	r.Route("/flags", func(r chi.Router) {
//	    r.Mount("/", (&flags.AdminAPI{Flags: store}).Routes())
//	})
type AdminAPI struct {
	Flags Reader
}

// Routes returns GET / (all flags, by key) and GET /{key} (one flag).
func (a *AdminAPI) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", a.list)
	r.Get("/{key}", a.get)
	return r
}

func (a *AdminAPI) list(w http.ResponseWriter, r *http.Request) {
	all, err := a.Flags.List(r.Context())
	if err != nil {
		httpx.JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	if all == nil {
		all = []Flag{}
	}
	httpx.JSON(w, http.StatusOK, all)
}

func (a *AdminAPI) get(w http.ResponseWriter, r *http.Request) {
	f, err := a.Flags.Get(r.Context(), chi.URLParam(r, "key"))
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.JSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case err != nil:
		httpx.JSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
	default:
		httpx.JSON(w, http.StatusOK, f)
	}
}
