package admin

import (
	"github.com/go-chi/chi/v5"

	"github.com/ManavA/keel/httpx/middleware"
)

// Router returns a chi.Router with:
//
//	POST /login     (rate-limited)
//	POST /refresh   (requires RequireAdmin)
//	GET  /audit     (requires RequireAdmin; the audit trail, oldest first)
//
// and this Service's CORS policy applied to every route, including ones a
// caller adds afterward. A caller mounts its own operator-only routes on the
// returned router, protecting them with RequireAdmin the same way /refresh
// is protected:
//
//	r := adminService.Router()
//	r.Group(func(r chi.Router) {
//	    r.Use(adminService.RequireAdmin)
//	    r.Get("/listings", listingsHandler)
//	})
func (s *Service) Router() chi.Router {
	r := chi.NewRouter()

	if s.corsOrigin != "" {
		r.Use(middleware.CORS(middleware.CORSOptions{
			AllowedOrigins:   []string{s.corsOrigin},
			AllowCredentials: true,
		}))
	}

	r.Group(func(r chi.Router) {
		r.Use(middleware.RealIP(s.realIP))
		r.Use(middleware.RateLimit(s.loginRateLimit))
		r.Post("/login", s.Login)
	})

	r.Group(func(r chi.Router) {
		r.Use(s.RequireAdmin)
		r.Post("/refresh", s.Refresh)
		r.Get("/audit", s.AuditList)
	})

	return r
}
