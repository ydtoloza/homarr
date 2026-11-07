// Package widgets is the Go port of the widget-domain tRPC routers of
// packages/api/src/router/widgets — today the media-organizer and downloads
// routers that the media-organizer and downloads widgets poll:
//
//	widget.mediaOrganizer.getData      (packages/api/src/router/widgets/media-organizer.ts:10)
//	widget.downloads.getJobsAndStatuses (packages/api/src/router/widgets/downloads.ts:50)
//	widget.downloads.pause / pauseItem / resume / resumeItem / deleteItem
//
// Procedures are registered under their exact upstream dotted paths (the
// widgetRouter of packages/api/src/router/widgets/index.ts mounts them under
// "widget.<domain>") so the Homarr frontend keeps calling the same endpoints.
//
// Upstream resolves the integrations of a request with
// createManyWidgetIntegrationMiddleware (packages/api/src/middlewares/
// integration.ts): it loads the rows from the "integration" table filtered by
// the widget's supported kinds, decrypts the secrets (decryptSecret,
// @homarr/encryption) and enforces query/interact permissions. homarr-go has
// no integration tables yet, so that step is the Options.ResolveIntegrations
// seam: the composition root injects the DB-backed resolver once M4 lands the
// tables, and tests inject fakes (see options.go and WIDGETS.md).
package widgets

import (
	"github.com/ydtoloza/homarr/server/internal/api/router/internal/admin"
	"github.com/ydtoloza/homarr/server/internal/api/trpc"
)

// Register registers the widget-domain procedures under their exact upstream
// paths. Call once at startup (Register panics on duplicate paths). Unlike
// the admin routers there is no *db.DB parameter: upstream's DB access for
// widget requests lives entirely inside the integration middleware, which
// this port injects through Options.ResolveIntegrations.
func Register(r *trpc.Router, opts Options) {
	s := newService(opts)
	registerMediaOrganizerRouter(r, s)
	registerDownloadsRouter(r, s)
}

// registerMediaOrganizerRouter mirrors mediaOrganizerRouter
// (packages/api/src/router/widgets/media-organizer.ts:9-47).
func registerMediaOrganizerRouter(r *trpc.Router, s *service) {
	r.Register("widget.mediaOrganizer.getData", s.mediaOrganizerGetData)
}

// registerDownloadsRouter mirrors downloadsRouter
// (packages/api/src/router/widgets/downloads.ts:49-194). Mutations are
// protectedProcedure (anonymous callers get UNAUTHORIZED) wrapped with
// admin.Writable, the port of the base credentialProcedure
// (packages/api/src/trpc.ts:130-135).
func registerDownloadsRouter(r *trpc.Router, s *service) {
	r.Register("widget.downloads.getJobsAndStatuses", s.downloadsGetJobsAndStatuses)
	r.Register("widget.downloads.pause", admin.Writable(s.protected(s.downloadsPause)))
	r.Register("widget.downloads.pauseItem", admin.Writable(s.protected(s.downloadsPauseItem)))
	r.Register("widget.downloads.resume", admin.Writable(s.protected(s.downloadsResume)))
	r.Register("widget.downloads.resumeItem", admin.Writable(s.protected(s.downloadsResumeItem)))
	r.Register("widget.downloads.deleteItem", admin.Writable(s.protected(s.downloadsDeleteItem)))
}
