package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/auth"
	"github.com/orderly/orderly-backend/internal/config"
	"github.com/orderly/orderly-backend/internal/confighttp"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/internal/customers"
	"github.com/orderly/orderly-backend/internal/menu"
	"github.com/orderly/orderly-backend/internal/orders"
	"github.com/orderly/orderly-backend/internal/payments"
	"github.com/orderly/orderly-backend/internal/platform"
	"github.com/orderly/orderly-backend/internal/secretbox"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/response"
)

type Server struct {
	log       *slog.Logger
	pool      *pgxpool.Pool
	cfg       config.Config
	auth      *auth.Service
	admin     *platform.Handler
	menu      *menu.Handler
	orders    *orders.Handler
	shop      *storefront.AdminHandler
	loader    *storefront.Loader
	customers *customers.Handler
	payments  *payments.Handler
	configs   *confighttp.Handler
}

func New(log *slog.Logger, pool *pgxpool.Pool, cfg config.Config) *Server {
	admin := platform.NewHandler(pool)

	// One configuration stack for the process: a single secretbox, store and
	// resolver, so a secret is never sealed under one key and looked for under
	// another.
	box := secretbox.New(cfg.ConfigEncryptionKey)
	store := configsvc.NewStore(sqlc.New(pool), box, slogAdapter{log})
	resolver := configsvc.NewResolver(store, slogAdapter{log})
	configService := configsvc.NewService(store, resolver, slogAdapter{log})
	admin.AttachConfigService(configService, box)
	admin.AttachLogger(log)

	if box.Disabled() {
		log.Warn("CONFIG_ENCRYPTION_KEY is not set: provider secrets cannot be stored. " +
			"Set it and restart before configuring SMTP, storage or AI credentials.")
	}

	authSvc := auth.NewService(pool, cfg)
	orderHandler := orders.NewHandler(pool, log)
	loader := storefront.NewLoader(pool)
	s := &Server{
		log: log, pool: pool, cfg: cfg,
		auth:      authSvc,
		admin:     admin,
		menu:      menu.NewHandler(pool),
		orders:    orderHandler,
		customers: customers.NewHandler(pool, orderHandler.Viewer(), loader, cfg, log),
		payments:  payments.NewHandler(pool, orderHandler, log),
		configs:   confighttp.NewHandler(configService, box, admin, log),
	}
	// The admin storefront handler needs the server to build public store URLs,
	// so it is attached after the server value exists.
	s.shop = storefront.NewAdminHandler(pool, s.publicStoreURL)
	return s
}

// publicStoreURL builds the customer-facing address for a tenant slug. The
// scheme follows the environment so a production build never advertises
// http:// links in a printed QR code.
func (s *Server) publicStoreURL(slug string) string {
	scheme := "https"
	if s.cfg.AppEnv == "development" || s.cfg.AppEnv == "dev" || s.cfg.AppEnv == "" {
		scheme = "http"
		port := s.cfg.FrontendPort
		if port != "" && port != "80" {
			scheme = "http"
			return storefront.StorefrontURL(slug, s.cfg.BaseDomain, scheme) + ":" + port
		}
	}
	return storefront.StorefrontURL(slug, s.cfg.BaseDomain, scheme)
}

// tenantIDOf returns the caller's tenant from the verified JWT. Handlers are
// given this rather than reading an id from the request, so a tenant admin
// cannot address another tenant's configuration.
func (s *Server) tenantIDOf(r *http.Request) *uuid.UUID {
	user, ok := identity.UserFromContext(r.Context())
	if !ok || user.TenantID == nil {
		return nil
	}
	return user.TenantID
}

// slogAdapter satisfies configsvc.Logger with the project's structured logger.
type slogAdapter struct{ log *slog.Logger }

func (a slogAdapter) Warn(msg string, args ...any) { a.log.Warn(msg, args...) }
func (a slogAdapter) Info(msg string, args ...any) { a.log.Info(msg, args...) }

// MigrateLegacyConfig moves a pre-existing plaintext SMTP password into the
// encrypted store. It is idempotent and runs on every boot.
func (s *Server) MigrateLegacyConfig(ctx context.Context) error {
	_, err := configsvc.MigrateLegacySMTP(ctx, sqlc.New(s.pool), secretbox.New(s.cfg.ConfigEncryptionKey), slogAdapter{s.log})
	return err
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))
	r.Use(cors.Handler(cors.Options{
		AllowedMethods:   []string{"GET", "POST", "PATCH", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: true,
		AllowOriginFunc:  s.allowOrigin,
		MaxAge:           300,
	}))
	r.Use(tenantctx.Middleware(s.pool, s.cfg.BaseDomain))

	r.Get("/health", s.handleHealth)
	r.Get("/health/ready", s.handleReady)

	r.Route("/api/v1", func(api chi.Router) {
		api.Route("/auth", func(ar chi.Router) {
			ar.Get("/admin/setup-status", s.auth.HandleAdminSetupStatus)
			ar.Post("/admin/setup", s.auth.HandleAdminSetup)
			ar.Post("/admin/login", s.auth.HandleAdminLogin)
			ar.Post("/tenant/login", s.auth.HandleTenantLogin)
			ar.Post("/setup-password", s.auth.HandleSetupPassword)
			ar.Post("/refresh", s.auth.HandleRefresh)
			ar.Post("/logout", s.auth.HandleLogout)
			ar.Group(func(pr chi.Router) {
				pr.Use(auth.Middleware(s.auth))
				pr.Get("/me", s.auth.HandleMe)
				pr.Put("/me/password", s.auth.HandleChangePassword)
			})
		})

		api.Route("/admin", func(ar chi.Router) {
			ar.Use(auth.Middleware(s.auth))
			ar.Use(auth.RequireRoles(identity.RoleSuperAdmin))
			ar.Get("/dashboard", s.admin.Dashboard)
			ar.Get("/settings", s.admin.Settings)
			ar.Patch("/settings", s.admin.PatchSettings)
			ar.Get("/theme-presets", s.admin.ListThemePresets)
			ar.Post("/theme-presets", s.admin.CreateThemePreset)
			ar.Patch("/theme-presets/{id}", s.admin.UpdateThemePreset)
			ar.Delete("/theme-presets/{id}", s.admin.DeleteThemePreset)
			ar.Get("/tenant-types", s.admin.ListTenantTypes)
			ar.Post("/tenant-types", s.admin.CreateTenantType)
			ar.Patch("/tenant-types/{code}", s.admin.UpdateTenantType)
			ar.Get("/plans", s.admin.ListAllPlans)
			ar.Post("/plans", s.admin.CreatePlan)
			ar.Patch("/plans/{id}", s.admin.UpdatePlan)
			ar.Get("/users", s.admin.ListPlatformUsers)
			ar.Post("/users", s.admin.CreateTenantUser)
			ar.Patch("/users/{id}", s.admin.UpdateTenantUser)
			ar.Get("/subscriptions", s.admin.ListSubscriptions)
			ar.Get("/tenants", s.admin.ListTenants)
			ar.Get("/tenants/slug-available", s.admin.SlugAvailable)
			ar.Post("/tenants", s.admin.CreateTenant)
			ar.Get("/tenants/{id}", s.admin.GetTenant)
			ar.Post("/tenants/{id}/resend-invite", s.admin.ResendTenantInvite)
			ar.Get("/tenants/{id}/metrics", s.admin.TenantMetrics)
			ar.Patch("/tenants/{id}", s.admin.UpdateTenant)
			ar.Patch("/tenants/{id}/theme", s.admin.UpdateTenantTheme)
			ar.Post("/tenants/{id}/change-plan", s.admin.ChangeTenantPlan)
			ar.Post("/tenants/{id}/activate", s.admin.ActivateTenant)
			ar.Post("/tenants/{id}/suspend", s.admin.SuspendTenant)
			ar.Get("/tenants/{id}/admins", s.admin.ListTenantAdmins)
			ar.Get("/tenants/{id}/users", s.admin.ListTenantUsers)
			ar.Post("/tenants/{id}/users", s.admin.CreateTenantUserForTenant)
			ar.Patch("/tenants/{id}/users/{userId}", s.admin.UpdateTenantUser)
			ar.Post("/tenants/{id}/users/{userId}/reset-access", s.admin.ResetTenantUserAccess)
			ar.Post("/tenants/{id}/users/{userId}/resend-invite", s.admin.ResendTenantUserInvite)
			ar.Get("/audit-logs", s.admin.ListAuditLogs)

			// Provider configuration, platform level.
			ar.Get("/configurations", s.configs.ListConfigurations)
			ar.Get("/configurations/{service}", s.configs.GetConfiguration)
			ar.Put("/configurations/{service}", s.configs.PutConfiguration)
			ar.Patch("/configurations/{service}/sharing", s.configs.SetPlatformSharing)
			ar.Post("/configurations/{service}/test", s.configs.TestConfiguration)
			ar.Post("/configurations/{service}/test-action", s.configs.TestConfigurationAction)
			ar.Get("/configurations/{service}/providers", s.configs.ListProviders)

			// Per-tenant platform-access control.
			ar.Get("/tenant-configurations", s.configs.ListAllTenantConfigurations)
			ar.Get("/tenants/{id}/configurations", s.configs.ListTenantConfigurations)
			ar.Put("/tenants/{id}/configurations/{service}/access", s.configs.SetTenantAccess)
		})

		api.Route("/tenant", func(tr chi.Router) {
			tr.Use(auth.Middleware(s.auth))
			tr.Use(auth.RequireRoles(identity.RoleTenantAdmin, identity.RoleStaff))
			tr.Use(auth.RequireTenant)
			tr.Use(auth.MatchHostTenant)

			tr.Get("/configurations", s.configs.ListTenantServicesFunc(s.tenantIDOf))
			tr.Get("/configurations/{service}", s.configs.GetTenantServiceFunc(s.tenantIDOf))
			tr.Put("/configurations/{service}", s.configs.PutTenantServiceFunc(s.tenantIDOf))
			tr.Delete("/configurations/{service}", s.configs.DeleteTenantServiceFunc(s.tenantIDOf))
			tr.Post("/configurations/{service}/test", s.configs.TestTenantServiceFunc(s.tenantIDOf))
			tr.Post("/configurations/{service}/test-action", s.configs.TestTenantServiceActionFunc(s.tenantIDOf))
			tr.Get("/configurations/{service}/effective", s.configs.GetEffectiveFunc(s.tenantIDOf))
			tr.Get("/service-preferences", s.configs.ListPreferencesFunc(s.tenantIDOf))
			tr.Put("/service-preferences/{service}", s.configs.SetPreferenceFunc(s.tenantIDOf))

			tr.Get("/theme", s.admin.GetMyTenantTheme)
			tr.Patch("/theme", s.admin.UpdateMyTenantTheme)
			tr.Get("/theme-presets", s.admin.ListThemePresets)
			tr.Get("/dashboard", s.menu.Dashboard)
			tr.Get("/analytics", s.menu.Analytics)
			tr.Get("/store-link", s.shop.StoreLink)
			tr.Get("/setup", s.menu.SetupStatus)
			tr.Post("/setup/complete-step", s.menu.CompleteSetupStep)
			tr.Post("/publish", s.menu.Publish)
			tr.Post("/unpublish", s.menu.Unpublish)

			tr.Get("/categories", s.menu.ListCategories)
			tr.Post("/categories", s.menu.CreateCategory)
			tr.Patch("/categories/{id}", s.menu.UpdateCategory)
			tr.Delete("/categories/{id}", s.menu.DeleteCategory)

			tr.Get("/products", s.menu.ListProducts)
			tr.Post("/products", s.menu.CreateProduct)
			tr.Get("/products/{id}", s.menu.GetProduct)
			tr.Patch("/products/{id}", s.menu.UpdateProduct)
			tr.Delete("/products/{id}", s.menu.DeleteProduct)

			tr.Get("/orders", s.orders.ListOrders)
			tr.Get("/orders/{id}", s.orders.GetOrder)
			tr.Post("/orders/{id}/accept", s.orders.Transition("accept"))
			tr.Post("/orders/{id}/prepare", s.orders.Transition("prepare"))
			tr.Post("/orders/{id}/ready", s.orders.Transition("ready"))
			tr.Post("/orders/{id}/complete", s.orders.Transition("complete"))
			tr.Post("/orders/{id}/cancel", s.orders.CancelStaffOrder)
			tr.Post("/payments/{id}/confirm", s.orders.ConfirmPayment)

			// Storefront configuration. The tenant comes from the verified
			// staff token and the request host, so one shop can never read or
			// write another's storefront.
			//
			// Owner-only, unlike the rest of this group. Staff take orders and work
			// the kitchen board; they do not get to rewrite the prices, tax,
			// packaging fee or payment rules they are then expected to collect, and
			// they cannot unpublish a live storefront. The list is short enough that
			// inline middleware is clearer than a second router.
			ownerOnly := auth.RequireRoles(identity.RoleTenantAdmin)

			tr.With(ownerOnly).Get("/storefront", s.shop.GetStorefront)
			tr.With(ownerOnly).Put("/storefront", s.shop.PutStorefront)
			tr.With(ownerOnly).Put("/storefront/theme", s.shop.PutTheme)
			tr.With(ownerOnly).Put("/storefront/homepage", s.shop.PutHomepage)
			tr.With(ownerOnly).Put("/storefront/hours", s.shop.PutOpeningHours)
			tr.With(ownerOnly).Get("/storefront/preview", s.shop.PreviewMenu)
			tr.With(ownerOnly).Get("/storefront/qr", s.shop.QRCode)

			// Payment methods and order workflow are separate documents within
			// the same configuration row, exposed at their own paths so the
			// admin screens and the API surface both read clearly. Owner-only for
			// the same reason as the rest of the storefront configuration.
			tr.With(ownerOnly).Get("/payment-settings", s.shop.GetStorefront)
			tr.With(ownerOnly).Put("/payment-settings", s.shop.PutPaymentSettings)
			tr.With(ownerOnly).Get("/order-workflow", s.shop.GetStorefront)
			tr.With(ownerOnly).Put("/order-workflow", s.shop.PutOrderWorkflow)
		})

		// Host-resolved public APIs (tenant subdomain required).
		//
		// Customer tokens are optional here rather than required: browsing,
		// guest checkout and guest order tracking all work with no token at
		// all, and a signed-in customer simply gets a richer response.
		api.Route("/public", func(pr chi.Router) {
			pr.Use(auth.OptionalCustomerMiddleware(s.auth))
			pr.Get("/store", s.orders.PublicStore)
			pr.Get("/menu", s.orders.PublicMenu)
			pr.Get("/products/{id}", s.orders.PublicProduct)
			pr.Post("/quote", s.orders.PublicQuote)
			pr.Post("/orders", s.orders.PublicCreateOrder)
			pr.Get("/orders/lookup", s.orders.PublicLookupOrder)
			pr.Get("/orders/{orderNumber}", s.orders.PublicTrackOrder)

			pr.Post("/orders/{orderNumber}/pay", s.payments.Start)
			pr.Post("/orders/{orderNumber}/pay/confirm", s.payments.Confirm)
			pr.Get("/orders/{orderNumber}/pay", s.payments.Status)
		})

		// Storefront customer identity. Reachable before an order exists, so
		// these routes need no token — the tenant still comes from the host.
		api.Route("/auth/customer", func(cr chi.Router) {
			cr.Post("/send-otp", s.customers.SendOtp)
			cr.Post("/verify-otp", s.customers.VerifyOtp)
		})

		api.Route("/customer", func(cr chi.Router) {
			cr.Use(auth.CustomerMiddleware(s.auth))
			cr.Get("/profile", s.customers.Profile)
			cr.Put("/profile", s.customers.UpdateProfile)
			cr.Get("/orders", s.customers.Orders)
			cr.Get("/orders/{orderNumber}", s.customers.SingleOrder)
			cr.Post("/logout", s.customers.Logout)
		})
	})

	return r
}

func (s *Server) allowOrigin(r *http.Request, origin string) bool {
	if origin == "" {
		return false
	}
	base := s.cfg.BaseDomain
	port := s.cfg.FrontendPort
	allowed := []string{
		"http://localhost:" + port,
		"http://127.0.0.1:" + port,
		"http://admin." + base + ":" + port,
		"http://api." + base + ":" + port,
	}
	for _, a := range allowed {
		if origin == a {
			return true
		}
	}
	// http://{slug}.localhost:5173
	prefix := "http://"
	suffix := "." + base + ":" + port
	if strings.HasPrefix(origin, prefix) && strings.HasSuffix(origin, suffix) {
		sub := strings.TrimSuffix(strings.TrimPrefix(origin, prefix), suffix)
		return sub != "" && sub != "www"
	}

	// Local Vite often hops ports (5173/5174/…) when one is busy.
	if s.cfg.AppEnv == "development" || s.cfg.AppEnv == "dev" || s.cfg.AppEnv == "" {
		if originIsLocalDev(origin, base) {
			return true
		}
	}
	return false
}

func originIsLocalDev(origin, baseDomain string) bool {
	u, err := parseHTTPOrigin(origin)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || host == "127.0.0.1" {
		return true
	}
	base := strings.ToLower(baseDomain)
	if host == "admin."+base || host == "api."+base {
		return true
	}
	suffix := "." + base
	if strings.HasSuffix(host, suffix) {
		sub := strings.TrimSuffix(host, suffix)
		return sub != "" && sub != "www"
	}
	return false
}

func parseHTTPOrigin(origin string) (*url.URL, error) {
	u, err := url.Parse(origin)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, url.InvalidHostError(origin)
	}
	return u, nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		s.log.Error("readiness check failed", "error", err)
		response.Error(w, http.StatusServiceUnavailable, "not_ready", "database unavailable")
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
