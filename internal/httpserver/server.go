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
	"github.com/orderly/orderly-backend/internal/appointments"
	"github.com/orderly/orderly-backend/internal/auth"
	"github.com/orderly/orderly-backend/internal/config"
	"github.com/orderly/orderly-backend/internal/confighttp"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/internal/customers"
	"github.com/orderly/orderly-backend/internal/hotel"
	"github.com/orderly/orderly-backend/internal/menu"
	"github.com/orderly/orderly-backend/internal/orders"
	"github.com/orderly/orderly-backend/internal/payments"
	"github.com/orderly/orderly-backend/internal/platform"
	"github.com/orderly/orderly-backend/internal/secretbox"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/internal/tables"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/response"
)

type Server struct {
	log          *slog.Logger
	pool         *pgxpool.Pool
	cfg          config.Config
	auth         *auth.Service
	admin        *platform.Handler
	menu         *menu.Handler
	upload       *menu.UploadHandler
	orders       *orders.Handler
	shop         *storefront.AdminHandler
	loader       *storefront.Loader
	customers    *customers.Handler
	payments     *payments.Handler
	configs      *confighttp.Handler
	appointments *appointments.Handler
	hotel        *hotel.Handler
	tables       *tables.Handler
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
	uploadHandler := menu.NewUploadHandler(configService.StorageService())

	if box.Disabled() {
		log.Warn("CONFIG_ENCRYPTION_KEY is not set: provider secrets cannot be stored. " +
			"Set it and restart before configuring SMTP, storage or AI credentials.")
	}

	authSvc := auth.NewService(pool, cfg)
	orderHandler := orders.NewHandler(pool, log)
	loader := storefront.NewLoader(pool)
	s := &Server{
		log: log, pool: pool, cfg: cfg,
		auth:         authSvc,
		admin:        admin,
		menu:         menu.NewHandler(pool),
		upload:       uploadHandler,
		orders:       orderHandler,
		customers:    customers.NewHandler(pool, orderHandler.Viewer(), loader, cfg, log),
		payments:     payments.NewHandler(pool, orderHandler, log),
		configs:      confighttp.NewHandler(configService, box, admin, log),
		appointments: appointments.NewHandler(pool),
		hotel:        hotel.NewHandler(pool),
		tables:       tables.NewHandler(pool),
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
	r.Use(s.requestLogger)
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
			// Three console roles, not one. Which of them may reach a given
			// screen is decided by the capability guards below rather than by
			// this list, so adding a role never means auditing every route.
			ar.Use(auth.RequireRoles(
				identity.RoleSuperAdmin, identity.RolePlatformAdmin, identity.RoleSupport,
			))

			var (
				canBusinesses = auth.RequirePermission(identity.PermPlatformBusinesses)
				canPlans      = auth.RequirePermission(identity.PermPlatformPlans)
				canProviders  = auth.RequirePermission(identity.PermPlatformProviders)
				canSettings   = auth.RequirePermission(identity.PermPlatformSettings)
				canConsoleIAM = auth.RequirePermission(identity.PermPlatformIAM)
				canMonitoring = auth.RequirePermission(identity.PermPlatformMonitoring)
			)

			/* ---------- Monitoring ---------- */
			ar.With(canMonitoring).Get("/dashboard", s.admin.Dashboard)
			ar.With(canMonitoring).Get("/audit-logs", s.admin.ListAuditLogs)
			// Live component status. It probes the API and the database and
			// reports the configuration stack's own recorded state for the
			// providers — it never dials a provider.
			ar.With(canMonitoring).Get("/system-health", s.admin.SystemHealth)

			/* ---------- Platform settings and catalogues ---------- */
			ar.With(canSettings).Get("/settings", s.admin.Settings)
			ar.With(canSettings).Patch("/settings", s.admin.PatchSettings)
			ar.With(canSettings).Post("/theme-presets", s.admin.CreateThemePreset)
			ar.With(canSettings).Patch("/theme-presets/{id}", s.admin.UpdateThemePreset)
			ar.With(canSettings).Delete("/theme-presets/{id}", s.admin.DeleteThemePreset)
			ar.With(canSettings).Post("/tenant-types", s.admin.CreateTenantType)
			ar.With(canSettings).Patch("/tenant-types/{code}", s.admin.UpdateTenantType)

			// A console operator's own appearance. Ungated for the same reason
			// its tenant twin is: it changes their screen and nobody else's.
			// The shared platform default is written through PATCH /settings,
			// which does require the settings capability.
			ar.Get("/me/appearance", s.admin.GetMyPlatformAppearance)
			ar.Patch("/me/appearance", s.admin.UpdateMyPlatformAppearance)
			ar.Delete("/me/appearance", s.admin.ResetMyPlatformAppearance)

			// Read-only catalogues. Every console screen that renders a business
			// needs these to label it, including the read-only ones.
			ar.Get("/theme-presets", s.admin.ListThemePresets)
			ar.Get("/tenant-types", s.admin.ListTenantTypes)
			ar.Get("/business-types/{code}/capabilities", s.admin.ListBusinessTypeCapabilities)

			/* ---------- Plans and subscriptions ---------- */
			ar.With(canPlans).Get("/plans", s.admin.ListAllPlans)
			ar.With(canPlans).Post("/plans", s.admin.CreatePlan)
			ar.With(canPlans).Patch("/plans/{id}", s.admin.UpdatePlan)
			ar.With(canPlans).Get("/subscriptions", s.admin.ListSubscriptions)
			ar.With(canPlans).Post("/tenants/{id}/change-plan", s.admin.ChangeTenantPlan)

			/* ---------- Businesses ----------
			 *
			 * The console owns the business *account*: who it is, what it pays
			 * for, and whether it is allowed to trade. It does not own the shop.
			 * There is deliberately no route here that writes a tenant's theme,
			 * storefront, menu, opening hours or store status — those belong to
			 * the people who run the business, on their own host.
			 */
			ar.With(canBusinesses).Get("/tenants", s.admin.ListTenants)
			ar.With(canBusinesses).Get("/tenants/slug-available", s.admin.SlugAvailable)
			ar.With(canBusinesses).Get("/tenants/email-available", s.admin.EmailAvailable)
			ar.With(canBusinesses).Post("/tenants", s.admin.CreateTenant)
			ar.With(canBusinesses).Get("/tenants/{id}", s.admin.GetTenant)
			ar.With(canBusinesses).Get("/tenants/{id}/metrics", s.admin.TenantMetrics)
			ar.With(canBusinesses).Patch("/tenants/{id}", s.admin.UpdateTenant)
			ar.With(canBusinesses).Post("/tenants/{id}/activate", s.admin.ActivateTenant)
			ar.With(canBusinesses).Post("/tenants/{id}/suspend", s.admin.SuspendTenant)
			// Recovering the owner's access is support, not staff management:
			// without it a business whose administrator lost their invitation
			// has no way back in.
			ar.With(canBusinesses).Post("/tenants/{id}/resend-invite", s.admin.ResendTenantInvite)
			ar.With(canBusinesses).Post("/upload", s.admin.UploadAsset)

			/* ---------- Console access ----------
			 *
			 * Who can reach this console. A business's own staff are invited
			 * and managed inside that business, by someone who works there —
			 * there is no route here that creates or edits a tenant user.
			 */
			ar.With(canConsoleIAM).Get("/users", s.admin.ListConsoleUsers)
			ar.With(canConsoleIAM).Post("/users", s.admin.CreateConsoleUser)
			ar.With(canConsoleIAM).Patch("/users/{id}", s.admin.UpdateConsoleUser)
			ar.With(canConsoleIAM).Post("/users/{id}/resend-invite", s.admin.ResendConsoleInvite)

			/* ---------- Providers ---------- */
			ar.With(canProviders).Get("/configurations", s.configs.ListConfigurations)
			ar.With(canProviders).Get("/configurations/{service}", s.configs.GetConfiguration)
			ar.With(canProviders).Put("/configurations/{service}", s.configs.PutConfiguration)
			ar.With(canProviders).Patch("/configurations/{service}/sharing", s.configs.SetPlatformSharing)
			ar.With(canProviders).Post("/configurations/{service}/test", s.configs.TestConfiguration)
			ar.With(canProviders).Post("/configurations/{service}/test-action", s.configs.TestConfigurationAction)
			ar.With(canProviders).Get("/configurations/{service}/providers", s.configs.ListProviders)

			// Per-business access to platform providers.
			ar.With(canProviders).Get("/tenant-configurations", s.configs.ListAllTenantConfigurations)
			ar.With(canProviders).Get("/tenants/{id}/configurations", s.configs.ListTenantConfigurations)
			ar.With(canProviders).Put("/tenants/{id}/configurations/{service}/access", s.configs.SetTenantAccess)
		})

		api.Route("/tenant", func(tr chi.Router) {
			tr.Use(auth.Middleware(s.auth))
			tr.Use(auth.RequireRoles(identity.RoleTenantAdmin, identity.RoleManager, identity.RoleStaff))
			tr.Use(auth.RequireTenant)
			tr.Use(auth.MatchHostTenant)

			// Capability guards. Routes name what they need rather than who may
			// call them, so adding a role is a change to one table
			// (pkg/identity/permissions.go) instead of a sweep through here.
			var (
				canSell        = auth.RequirePermission(identity.PermSelling)
				canKitchen     = auth.RequirePermission(identity.PermKitchen)
				canMenu        = auth.RequirePermission(identity.PermMenu)
				canCustomers   = auth.RequirePermission(identity.PermCustomers)
				canStaff       = auth.RequirePermission(identity.PermStaff)
				canStorefront  = auth.RequirePermission(identity.PermStorefront)
				canOrg         = auth.RequirePermission(identity.PermOrganization)
				canIntegration = auth.RequirePermission(identity.PermIntegrations)
				canAnalytics   = auth.RequirePermission(identity.PermAnalytics)
				canActivity    = auth.RequirePermission(identity.PermActivity)
				canIAM         = auth.RequirePermission(identity.PermIAM)
			)

			// Business capability guards. A permission says who inside the
			// business may touch a module; a capability says whether the module
			// exists for this business type at all — a Hotel tenant's own owner
			// token still 403s on /orders, because there is no (HOTEL, ORDERS)
			// row for this tenant to have been granted. See
			// planing/business_saas_lld.md §2.
			var (
				reqCatalog      = tenantctx.RequireCapability(tenantctx.CapCatalog)
				reqOrders       = tenantctx.RequireCapability(tenantctx.CapOrders)
				reqCustomers    = tenantctx.RequireCapability(tenantctx.CapCustomers)
				reqStaff        = tenantctx.RequireCapability(tenantctx.CapStaff)
				reqServices     = tenantctx.RequireCapability(tenantctx.CapServices)
				reqAppointments = tenantctx.RequireCapability(tenantctx.CapAppointments)
				reqQueue        = tenantctx.RequireCapability(tenantctx.CapQueue)
				reqRooms        = tenantctx.RequireCapability(tenantctx.CapRooms)
				reqReservations = tenantctx.RequireCapability(tenantctx.CapReservations)
				reqHousekeeping = tenantctx.RequireCapability(tenantctx.CapHousekeeping)
				reqTables       = tenantctx.RequireCapability(tenantctx.CapTables)
			)

			tr.With(canIntegration).Get("/configurations", s.configs.ListTenantServicesFunc(s.tenantIDOf))
			tr.With(canIntegration).Get("/configurations/{service}", s.configs.GetTenantServiceFunc(s.tenantIDOf))
			tr.With(canIntegration).Put("/configurations/{service}", s.configs.PutTenantServiceFunc(s.tenantIDOf))
			tr.With(canIntegration).Delete("/configurations/{service}", s.configs.DeleteTenantServiceFunc(s.tenantIDOf))
			tr.With(canIntegration).Post("/configurations/{service}/test", s.configs.TestTenantServiceFunc(s.tenantIDOf))
			tr.With(canIntegration).Post("/configurations/{service}/test-action", s.configs.TestTenantServiceActionFunc(s.tenantIDOf))
			tr.With(canIntegration).Get("/configurations/{service}/effective", s.configs.GetEffectiveFunc(s.tenantIDOf))
			tr.With(canIntegration).Get("/service-preferences", s.configs.ListPreferencesFunc(s.tenantIDOf))
			tr.With(canIntegration).Put("/service-preferences/{service}", s.configs.SetPreferenceFunc(s.tenantIDOf))

			// Read-only shell data. Every signed-in member of the business needs
			// these to render the console at all, including staff.
			tr.Get("/theme", s.admin.GetMyTenantTheme)
			tr.Get("/theme-presets", s.admin.ListThemePresets)
			tr.Get("/store-link", s.shop.StoreLink)
			tr.Get("/setup", s.menu.SetupStatus)

			// A person's own console appearance. Deliberately ungated: it
			// changes their screen and nobody else's, so requiring a
			// configuration capability would mean a staff member cannot pick
			// dark mode while an owner can rewrite the storefront. Writing the
			// *business* default is the owner-only PATCH /theme below.
			tr.Get("/me/appearance", s.admin.GetMyConsoleAppearance)
			tr.Patch("/me/appearance", s.admin.UpdateMyConsoleAppearance)
			tr.Delete("/me/appearance", s.admin.ResetMyConsoleAppearance)

			tr.With(canStorefront).Patch("/theme", s.admin.UpdateMyTenantTheme)
			tr.With(canAnalytics).Get("/dashboard", s.menu.Dashboard)
			tr.With(canAnalytics).Get("/analytics", s.menu.Analytics)
			tr.With(canStorefront).Post("/setup/complete-step", s.menu.CompleteSetupStep)
			tr.With(canStorefront).Post("/publish", s.menu.Publish)
			tr.With(canStorefront).Post("/unpublish", s.menu.Unpublish)

			// Activity and audit: the business's own history.
			tr.With(canActivity).Get("/order-history", s.orders.OrderHistory)
			tr.With(canActivity).Get("/activity", s.orders.ActivityFeed)
			tr.With(canActivity).Get("/audit-logs", s.admin.ShopAuditLogs)

			// Access control.
			tr.With(canIAM).Get("/iam", s.admin.ShopIAM)

			tr.With(canMenu, reqCatalog).Get("/categories", s.menu.ListCategories)
			tr.With(canMenu, reqCatalog).Post("/categories", s.menu.CreateCategory)
			tr.With(canMenu, reqCatalog).Post("/categories/reorder", s.menu.ReorderCategories)
			tr.With(canMenu, reqCatalog).Patch("/categories/{id}", s.menu.UpdateCategory)
			tr.With(canMenu, reqCatalog).Delete("/categories/{id}", s.menu.DeleteCategory)

			tr.With(canMenu, reqCatalog).Get("/products", s.menu.ListProducts)
			tr.With(canMenu, reqCatalog).Post("/products", s.menu.CreateProduct)
			tr.With(canMenu, reqCatalog).Post("/products/reorder", s.menu.ReorderProducts)
			tr.With(canMenu, reqCatalog).Get("/products/{id}", s.menu.GetProduct)
			tr.With(canMenu, reqCatalog).Post("/products/{id}/duplicate", s.menu.DuplicateProduct)
			tr.With(canMenu, reqCatalog).Patch("/products/{id}", s.menu.UpdateProduct)
			tr.With(canMenu, reqCatalog).Delete("/products/{id}", s.menu.DeleteProduct)

			// Image upload for menu items, so it follows the menu permission and
			// capability.
			tr.Group(func(ur chi.Router) {
				ur.Use(canMenu, reqCatalog)
				s.upload.UploadRoutes(ur)
			})

			tr.With(canSell, reqOrders).Get("/orders", s.orders.ListOrders)
			tr.With(canSell, reqOrders).Post("/orders", s.orders.StaffCreateOrder)
			tr.With(canSell, reqOrders).Get("/orders/{id}", s.orders.GetOrder)
			tr.With(canSell, reqOrders).Post("/orders/{id}/accept", s.orders.Transition("accept"))
			tr.With(canSell, reqOrders).Post("/orders/{id}/prepare", s.orders.Transition("prepare"))
			tr.With(canSell, reqOrders).Post("/orders/{id}/ready", s.orders.Transition("ready"))
			tr.With(canSell, reqOrders).Post("/orders/{id}/complete", s.orders.Transition("complete"))
			tr.With(canSell, reqOrders).Post("/orders/{id}/cancel", s.orders.CancelStaffOrder)
			tr.With(canSell, reqOrders).Post("/payments/{id}/confirm", s.orders.ConfirmPayment)

			// Staff and customers. A manager runs both; staff reach neither.
			// CUSTOMERS/STAFF are "Yes" for every business type in the capability
			// matrix, so these guards never actually block a real tenant today —
			// they exist so that stays true by construction, not by convention.
			tr.With(canStaff, reqStaff).Get("/users", s.admin.ShopListUsers)
			tr.With(canStaff, reqStaff).Post("/users", s.admin.ShopCreateUser)
			tr.With(canStaff, reqStaff).Patch("/users/{userId}", s.admin.ShopUpdateUser)
			tr.With(canStaff, reqStaff).Post("/users/{userId}/reset-access", s.admin.ShopResetUserAccess)
			tr.With(canStaff, reqStaff).Post("/users/{userId}/resend-invite", s.admin.ShopResendUserInvite)

			tr.With(canCustomers, reqCustomers).Get("/customers", s.customers.ShopListCustomers)
			tr.With(canCustomers, reqCustomers).Get("/customers/guest", s.customers.ShopGetGuestCustomer)
			tr.With(canCustomers, reqCustomers).Get("/customers/{id}", s.customers.ShopGetCustomer)
			tr.With(canCustomers, reqCustomers).Post("/customers/{id}/block", s.customers.ShopSetCustomerBlocked)

			/* ---------- Barber Shop: services, appointments, queue ---------- */
			tr.With(canMenu, reqServices).Get("/services", s.appointments.ListServices)
			tr.With(canMenu, reqServices).Post("/services", s.appointments.CreateService)
			tr.With(canMenu, reqServices).Patch("/services/{id}", s.appointments.UpdateService)
			tr.With(canMenu, reqServices).Delete("/services/{id}", s.appointments.DeleteService)

			tr.With(canStaff, reqServices).Get("/staff-availability", s.appointments.ListStaffAvailability)
			tr.With(canStaff, reqServices).Post("/staff-availability", s.appointments.CreateStaffAvailability)
			tr.With(canStaff, reqServices).Delete("/staff-availability/{id}", s.appointments.DeleteStaffAvailability)

			tr.With(canSell, reqAppointments).Get("/appointments", s.appointments.ListAppointments)
			tr.With(canSell, reqAppointments).Post("/appointments", s.appointments.CreateAppointment)
			tr.With(canSell, reqAppointments).Get("/appointments/{id}", s.appointments.GetAppointment)
			tr.With(canSell, reqAppointments).Post("/appointments/{id}/confirm", s.appointments.Transition("confirm"))
			tr.With(canSell, reqAppointments).Post("/appointments/{id}/checkin", s.appointments.Transition("checkin"))
			tr.With(canSell, reqAppointments).Post("/appointments/{id}/cancel", s.appointments.Transition("cancel"))
			tr.With(canSell, reqAppointments).Post("/appointments/{id}/no-show", s.appointments.Transition("noshow"))

			tr.With(canSell, reqQueue).Get("/queue", s.appointments.ListQueue)
			tr.With(canSell, reqQueue).Post("/queue", s.appointments.JoinQueue)
			tr.With(canSell, reqQueue).Post("/queue/{id}/call", s.appointments.QueueTransition("call"))
			tr.With(canSell, reqQueue).Post("/queue/{id}/start", s.appointments.QueueTransition("start"))
			tr.With(canSell, reqQueue).Post("/queue/{id}/complete", s.appointments.QueueTransition("complete"))
			tr.With(canSell, reqQueue).Post("/queue/{id}/cancel", s.appointments.QueueTransition("cancel"))

			/* ---------- Hotel: rooms, reservations, folios, housekeeping ---------- */
			tr.With(canMenu, reqRooms).Get("/room-types", s.hotel.ListRoomTypes)
			tr.With(canMenu, reqRooms).Post("/room-types", s.hotel.CreateRoomType)
			tr.With(canMenu, reqRooms).Patch("/room-types/{id}", s.hotel.UpdateRoomType)
			tr.With(canMenu, reqRooms).Delete("/room-types/{id}", s.hotel.DeleteRoomType)

			tr.With(canMenu, reqRooms).Get("/rooms", s.hotel.ListRooms)
			tr.With(canMenu, reqRooms).Post("/rooms", s.hotel.CreateRoom)
			tr.With(canSell, reqRooms).Post("/rooms/{id}/status", s.hotel.SetRoomStatus)

			tr.With(canSell, reqReservations).Get("/reservations", s.hotel.ListReservations)
			tr.With(canSell, reqReservations).Post("/reservations", s.hotel.CreateReservation)
			tr.With(canSell, reqReservations).Get("/reservations/{id}", s.hotel.GetReservation)
			tr.With(canSell, reqReservations).Post("/reservations/{id}/confirm", s.hotel.Transition("confirm"))
			tr.With(canSell, reqReservations).Post("/reservations/{id}/cancel", s.hotel.Transition("cancel"))
			tr.With(canSell, reqReservations).Post("/reservations/{id}/no-show", s.hotel.Transition("noshow"))
			tr.With(canSell, reqReservations).Post("/reservations/{id}/check-in", s.hotel.CheckIn)
			tr.With(canSell, reqReservations).Post("/reservations/{id}/check-out", s.hotel.CheckOut)
			tr.With(canSell, reqReservations).Get("/reservations/{id}/folio", s.hotel.GetFolio)
			tr.With(canSell, reqReservations).Post("/reservations/{id}/folio/charges", s.hotel.CreateCharge)

			tr.With(canKitchen, reqHousekeeping).Get("/housekeeping", s.hotel.ListHousekeepingTasks)
			tr.With(canKitchen, reqHousekeeping).Post("/housekeeping", s.hotel.CreateHousekeepingTask)
			tr.With(canKitchen, reqHousekeeping).Post("/housekeeping/{id}/complete", s.hotel.CompleteHousekeepingTask)

			/* ---------- Cafe/Restaurant: dine-in tables ---------- */
			tr.With(canMenu, reqTables).Get("/tables", s.tables.List)
			tr.With(canMenu, reqTables).Post("/tables", s.tables.Create)
			tr.With(canMenu, reqTables).Patch("/tables/{id}", s.tables.Update)
			tr.With(canSell, reqTables).Post("/tables/{id}/status", s.tables.SetStatus)
			tr.With(canMenu, reqTables).Delete("/tables/{id}", s.tables.Delete)

			// Storefront configuration. The tenant comes from the verified
			// staff token and the request host, so one shop can never read or
			// write another's storefront.
			//
			// Owner-only, unlike the rest of this group. Staff take orders and work
			// the kitchen board; they do not get to rewrite the prices, tax,
			// packaging fee or payment rules they are then expected to collect, and
			// they cannot unpublish a live storefront. The list is short enough that
			// inline middleware is clearer than a second router.
			tr.With(canStorefront).Get("/storefront", s.shop.GetStorefront)
			tr.With(canStorefront).Put("/storefront", s.shop.PutStorefront)
			tr.With(canStorefront).Put("/storefront/theme", s.shop.PutTheme)
			tr.With(canStorefront).Put("/storefront/homepage", s.shop.PutHomepage)
			tr.With(canStorefront).Put("/storefront/hours", s.shop.PutOpeningHours)
			tr.With(canStorefront).Get("/storefront/preview", s.shop.PreviewMenu)
			tr.With(canStorefront).Get("/storefront/qr", s.shop.QRCode)

			// Route aliases for /customize. Admin clients must call these on the
			// tenant Host ({slug}.{baseDomain}), never on the bare API host —
			// MatchHostTenant rejects cross-host tokens.
			tr.With(canStorefront).Get("/customize", s.shop.GetStorefront)
			tr.With(canStorefront).Put("/customize", s.shop.PutStorefront)
			tr.With(canStorefront).Put("/customize/theme", s.shop.PutTheme)
			tr.With(canStorefront).Put("/customize/homepage", s.shop.PutHomepage)
			tr.With(canStorefront).Put("/customize/hours", s.shop.PutOpeningHours)
			tr.With(canOrg).Put("/customize/payments", s.shop.PutPaymentSettings)
			tr.With(canOrg).Put("/customize/workflow", s.shop.PutOrderWorkflow)
			// The Studio's draft: a working copy that belongs to the shop
			// rather than to one browser, and a publish that is one
			// transaction rather than six writes with no way back.
			tr.With(canStorefront).Get("/customize/draft", s.shop.GetDraft)
			tr.With(canStorefront).Put("/customize/draft", s.shop.PutDraft)
			tr.With(canStorefront).Delete("/customize/draft", s.shop.DeleteDraft)
			tr.With(canStorefront).Post("/customize/draft/publish", s.shop.PublishDraft)

			tr.With(canStorefront).Get("/customize/preview", s.shop.PreviewMenu)
			tr.With(canStorefront).Get("/customize/qr", s.shop.QRCode)

			// Payment methods and order workflow are separate documents within
			// the same configuration row, exposed at their own paths so the
			// admin screens and the API surface both read clearly. Owner-only for
			// the same reason as the rest of the storefront configuration.
			tr.With(canOrg).Get("/payment-settings", s.shop.GetStorefront)
			tr.With(canOrg).Put("/payment-settings", s.shop.PutPaymentSettings)
			tr.With(canOrg).Get("/order-workflow", s.shop.GetStorefront)
			tr.With(canOrg).Put("/order-workflow", s.shop.PutOrderWorkflow)
		})

		// Host-resolved public APIs (tenant subdomain required).
		//
		// Customer tokens are optional here rather than required: browsing,
		// guest checkout and guest order tracking all work with no token at
		// all, and a signed-in customer simply gets a richer response.
		api.Route("/public", func(pr chi.Router) {
			pr.Use(auth.OptionalCustomerMiddleware(s.auth))

			reqCatalog := tenantctx.RequireCapability(tenantctx.CapCatalog)
			reqOrders := tenantctx.RequireCapability(tenantctx.CapOrders)

			pr.Get("/store", s.orders.PublicStore)

			// The console brand theme, public.
			//
			// A sign-in page has to look like the shop it belongs to, and it
			// renders precisely when nobody is signed in — so it cannot read the
			// authenticated /tenant/theme. This is the same handler on the same
			// host-resolved tenant, not a weaker one.
			//
			// Safe to expose because the payload is presentation only: a preset id,
			// a colour mode, and CSS values (two accents, three radii, two font
			// family names). No secret, no configuration, and no cross-tenant read:
			// the tenant comes from the hostname, so this returns the caller's own
			// shop's theme or nothing.
			pr.Get("/theme", s.admin.GetMyTenantTheme)

			pr.With(reqCatalog).Get("/menu", s.orders.PublicMenu)
			pr.With(reqCatalog).Get("/products/{id}", s.orders.PublicProduct)
			pr.With(reqOrders).Post("/quote", s.orders.PublicQuote)
			pr.With(reqOrders).Post("/orders", s.orders.PublicCreateOrder)
			pr.With(reqOrders).Get("/orders/lookup", s.orders.PublicLookupOrder)
			pr.With(reqOrders).Get("/orders/{orderNumber}", s.orders.PublicTrackOrder)

			pr.With(reqOrders).Post("/orders/{orderNumber}/pay", s.payments.Start)
			pr.With(reqOrders).Post("/orders/{orderNumber}/pay/confirm", s.payments.Confirm)
			pr.With(reqOrders).Get("/orders/{orderNumber}/pay", s.payments.Status)
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
