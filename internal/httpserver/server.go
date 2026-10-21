package httpserver

import (
	"context"
	"crypto/subtle"
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
	"github.com/orderly/orderly-backend/internal/attendance"
	"github.com/orderly/orderly-backend/internal/auth"
	"github.com/orderly/orderly-backend/internal/config"
	"github.com/orderly/orderly-backend/internal/confighttp"
	"github.com/orderly/orderly-backend/internal/configsvc"
	"github.com/orderly/orderly-backend/internal/customers"
	"github.com/orderly/orderly-backend/internal/entitlement"
	"github.com/orderly/orderly-backend/internal/hotel"
	"github.com/orderly/orderly-backend/internal/inventory"
	"github.com/orderly/orderly-backend/internal/marketing"
	"github.com/orderly/orderly-backend/internal/media"
	"github.com/orderly/orderly-backend/internal/menu"
	"github.com/orderly/orderly-backend/internal/notify"
	"github.com/orderly/orderly-backend/internal/orders"
	"github.com/orderly/orderly-backend/internal/payments"
	"github.com/orderly/orderly-backend/internal/platform"
	"github.com/orderly/orderly-backend/internal/promo"
	"github.com/orderly/orderly-backend/internal/publicurl"
	"github.com/orderly/orderly-backend/internal/secretbox"
	"github.com/orderly/orderly-backend/internal/signin"
	"github.com/orderly/orderly-backend/internal/storefront"
	"github.com/orderly/orderly-backend/internal/tables"
	"github.com/orderly/orderly-backend/internal/tenantctx"
	"github.com/orderly/orderly-backend/pkg/identity"
	"github.com/orderly/orderly-backend/pkg/netutil"
	"github.com/orderly/orderly-backend/pkg/response"
)

type Server struct {
	log           *slog.Logger
	pool          *pgxpool.Pool
	cfg           config.Config
	auth          *auth.Service
	admin         *platform.Handler
	menu          *menu.Handler
	upload        *menu.UploadHandler
	media         *media.Handler
	orders        *orders.Handler
	shop          *storefront.AdminHandler
	loader        *storefront.Loader
	customers     *customers.Handler
	payments      *payments.Handler
	paymentMode   *payments.AdminHandler
	paymentSvc    *payments.Service
	payLimits     payLimits
	configs       *confighttp.Handler
	appointments  *appointments.Handler
	hotel         *hotel.Handler
	tables        *tables.Handler
	marketing     *marketing.Handler
	promo         *promo.Handler
	configService *configsvc.Service
}

func New(log *slog.Logger, pool *pgxpool.Pool, cfg config.Config) *Server {
	admin := platform.NewHandler(pool, cfg)

	// One configuration stack for the process: a single secretbox, store and
	// resolver, so a secret is never sealed under one key and looked for under
	// another.
	box := secretbox.New(cfg.ConfigEncryptionKey)
	store := configsvc.NewStore(sqlc.New(pool), box, slogAdapter{log})
	resolver := configsvc.NewResolver(store, slogAdapter{log})
	configService := configsvc.NewService(store, resolver, slogAdapter{log})
	admin.AttachConfigService(configService, box)
	admin.AttachLogger(log)
	apiOrigin := publicurl.APIOrigin(cfg.BaseDomain, cfg.AppEnv, cfg.HTTPAddr)
	uploadHandler := menu.NewUploadHandler(configService.StorageService(), apiOrigin)
	mediaHandler := media.NewHandler(configService.StorageService())

	if box.Disabled() {
		log.Warn("CONFIG_ENCRYPTION_KEY is not set: provider secrets cannot be stored. " +
			"Set it and restart before configuring SMTP, storage or AI credentials.")
	}

	authSvc := auth.NewService(pool, cfg, log)
	authSvc.AttachNotifications(configService.Notifications())
	signIn := signin.NewService(pool, configService, cfg.AppEnv == "production" || cfg.AppEnv == "prod", cfg.SignInTestFallback)
	orderHandler := orders.NewHandler(pool, log)
	paymentService := payments.NewService(pool, orderHandler.PayableSource(), configService, cfg.PaymentsV2, sandboxSecret(cfg), log)
	orderHandler.SetPayments(paymentService)
	loader := storefront.NewLoader(pool)
	s := &Server{
		log: log, pool: pool, cfg: cfg,
		auth:          authSvc,
		admin:         admin,
		upload:        uploadHandler,
		media:         mediaHandler,
		orders:        orderHandler,
		customers:     customers.NewHandler(pool, orderHandler.Viewer(), loader, cfg, log),
		payments:      payments.NewHandler(pool, orderHandler.PayableSource(), paymentService, log),
		paymentMode:   payments.NewAdminHandler(paymentService, admin),
		paymentSvc:    paymentService,
		payLimits:     newPayLimits(cfg.PaymentsRateLimit),
		configs:       confighttp.NewHandler(configService, box, admin, log),
		appointments:  appointments.NewHandler(pool, log),
		hotel:         hotel.NewHandler(pool, log),
		tables:        tables.NewHandler(pool),
		configService: configService,
	}
	// Menu + storefront admin need the server's public URL builder, so they are
	// attached after the server value exists.
	s.menu = menu.NewHandler(pool, s.publicStoreURL)
	s.shop = storefront.NewAdminHandler(pool, s.publicStoreURL)
	s.shop.AttachSignIn(signIn)
	s.customers.AttachSignIn(signIn)
	s.orders.AttachSignIn(signIn)

	// Marketing links and unsubscribe pages are served from the API origin;
	// a link's destination is always the tenant's own storefront.
	s.marketing = marketing.NewHandler(pool, configService, s.marketingURLs(), log)
	s.promo = promo.NewHandler(pool, log)

	// Image links saved before media URLs existed (expiring or plain-http
	// storage links) are rewritten once, in the background, so they show
	// again without anyone re-uploading.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if n, err := media.Repair(ctx, pool, apiOrigin); err != nil {
			log.Warn("media: could not repair image links", "error", err)
		} else if n > 0 {
			log.Info("media: rewrote image links to media URLs", "count", n)
		}
	}()
	return s
}

// marketingURLs is the public routing the marketing engine puts in messages.
func (s *Server) marketingURLs() marketing.URLs {
	return marketing.URLs{
		APIOrigin: publicurl.APIOrigin(s.cfg.BaseDomain, s.cfg.AppEnv, s.cfg.HTTPAddr),
		StoreURL:  s.publicStoreURL,
	}
}

// StartMarketingWorker runs the campaign lifecycle (materialise, deliver,
// finish) until ctx is cancelled. Call it in a goroutine.
func (s *Server) StartMarketingWorker(ctx context.Context) {
	marketing.NewWorker(sqlc.New(s.pool), s.configService, s.marketingURLs(), s.log).Start(ctx, 10*time.Second)
}

// publicStoreURL builds the customer-facing address for a tenant slug. The
// scheme follows the environment so a production build never advertises
// http:// links in a printed QR code.
func (s *Server) publicStoreURL(slug string) string {
	return publicurl.TenantFrontendURL(slug, s.cfg.BaseDomain, s.cfg.AppEnv, s.cfg.FrontendPort)
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

// StartNotificationWorker runs the EMAIL/SMS delivery loop until ctx is
// cancelled. Call it in a goroutine: it blocks for the life of ctx.
func (s *Server) StartNotificationWorker(ctx context.Context) {
	notify.StartWorker(ctx, sqlc.New(s.pool), s.log, s.configService, 15*time.Second)
}

// StartPaymentSweeper expires payments whose time ran out, after first asking
// the gateway whether a customer paid but the confirmation was lost.
func (s *Server) StartPaymentSweeper(ctx context.Context) {
	s.paymentSvc.RunSweeper(ctx, time.Minute)
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(netutil.Middleware)
	r.Use(s.requestLogger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5))
	r.Use(middleware.Timeout(60 * time.Second))
	r.Use(cors.Handler(cors.Options{
		AllowedMethods:   []string{"GET", "POST", "PATCH", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", tenantctx.TenantSlugHeader},
		AllowCredentials: true,
		AllowOriginFunc:  s.allowOrigin,
		MaxAge:           300,
	}))
	r.Use(tenantctx.Middleware(s.pool, s.cfg.BaseDomain))

	// Gateway callbacks: no tenant host, no session. The provider signature is
	// the credential.
	r.With(wrap(s.payLimits.webhook)).Post("/api/v1/webhooks/payments/{provider}", s.payments.Webhook)

	r.Get("/health", s.handleHealth)

	// Marketing: trackable-link redirect and unsubscribe. No tenant host and
	// no session: the code / recipient id is the credential and names the
	// tenant. Mounted outside /api/v1/public because that group requires a
	// tenant host, and these are opened from an email or SMS.
	r.Get("/api/v1/go/{code}", s.marketing.Go)
	r.Get("/api/v1/marketing/unsubscribe/{id}", s.marketing.Unsubscribe)
	// Meta (WhatsApp) webhook: verify handshake, then signed deliveries.
	r.Get("/api/v1/webhooks/meta", s.marketing.WebhookVerify)
	r.Post("/api/v1/webhooks/meta", s.marketing.Webhook)
	r.Get("/health/ready", s.handleReady)

	r.Route("/api/v1", func(api chi.Router) {
		// Uploaded images, fetched anonymously by pages and mail clients.
		api.Get("/media/{scope}/*", s.media.Serve)
		// Email logos, fetched anonymously by mail clients (see EmailAsset).
		api.Get("/email-assets/{scope}/{name}", s.admin.EmailAsset)

		api.Route("/auth", func(ar chi.Router) {
			ar.Get("/admin/setup-status", s.auth.HandleAdminSetupStatus)
			ar.Post("/admin/setup", s.auth.HandleAdminSetup)
			ar.Post("/admin/login", s.auth.HandleAdminLogin)
			ar.Post("/tenant/login", s.auth.HandleTenantLogin)
			ar.Post("/setup-password", s.auth.HandleSetupPassword)
			ar.Post("/refresh", s.auth.HandleRefresh)
			ar.Post("/logout", s.auth.HandleLogout)
			// The challenge token itself is the credential here: the caller
			// has not finished logging in yet, so this sits outside auth.Middleware.
			ar.Post("/mfa/verify", s.auth.HandleMFAVerify)
			ar.Post("/mfa/challenge/email/send", s.auth.HandleMFAChallengeEmailSend)
			ar.Group(func(pr chi.Router) {
				pr.Use(auth.Middleware(s.auth))
				pr.Get("/me", s.auth.HandleMe)
				pr.Patch("/me", s.auth.HandleUpdateMe)
				pr.Put("/me/password", s.auth.HandleChangePassword)
				pr.Get("/me/mfa/status", s.auth.HandleMFAStatus)
				pr.Post("/me/mfa/setup", s.auth.HandleMFASetup)
				pr.Post("/me/mfa/confirm", s.auth.HandleMFAConfirm)
				pr.Post("/me/mfa/disable", s.auth.HandleMFADisable)
				pr.Post("/me/mfa/remove-method", s.auth.HandleMFARemoveMethod)
				pr.Post("/me/mfa/email/send", s.auth.HandleMFAEmailSend)
				pr.Post("/me/mfa/email/confirm", s.auth.HandleMFAEmailConfirm)
				pr.Post("/me/mfa/recovery-codes/regenerate", s.auth.HandleMFARecoveryCodesRegenerate)
			})
			// Forced enrollment: a user with zero methods under REQUIRED policy
			// past its grace period. The enrollment token itself is the
			// credential: like /mfa/verify, this sits outside auth.Middleware.
			ar.Post("/mfa/enroll/start", s.auth.HandleMFAEnrollStart)
			ar.Post("/mfa/enroll/confirm", s.auth.HandleMFAEnrollConfirm)
			ar.Post("/mfa/enroll/email/send", s.auth.HandleMFAEnrollEmailSend)
			ar.Post("/mfa/enroll/email/confirm", s.auth.HandleMFAEnrollEmailConfirm)
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
			// providers: it never dials a provider.
			ar.With(canMonitoring).Get("/system-health", s.admin.SystemHealth)

			// Notifications: every signed-in console user sees their own: no
			// extra capability, the router's RequireRoles above is the gate.
			ar.Get("/notifications", s.admin.ListNotifications)
			ar.Post("/notifications/{id}/read", s.admin.MarkNotificationRead)
			ar.Post("/notifications/{id}/unread", s.admin.MarkNotificationUnread)
			ar.Post("/notifications/read-all", s.admin.MarkAllNotificationsRead)
			ar.Get("/me/notification-preferences", s.admin.GetMyNotificationPreferences)
			ar.Put("/me/notification-preferences", s.admin.PutMyNotificationPreferences)

			// Platform notification settings: catalog, platform-default rules
			// and templates, and the full delivery log. Monitoring, not
			// Providers: this is "what the platform sends," not credentials.
			ar.With(canMonitoring).Get("/notification-events", s.admin.ListNotificationEvents)
			ar.With(canMonitoring).Get("/notification-rules", s.admin.ListPlatformNotificationRules)
			ar.With(canMonitoring).Put("/notification-rules", s.admin.PutPlatformNotificationRule)
			ar.With(canMonitoring).Get("/notification-templates", s.admin.ListPlatformNotificationTemplates)
			ar.With(canMonitoring).Put("/notification-templates", s.admin.PutPlatformNotificationTemplate)
			ar.With(canMonitoring).Post("/notification-templates/test-send", s.admin.TestSendPlatformNotificationTemplate)
			ar.With(canMonitoring).Post("/notification-templates/preview", s.admin.PreviewPlatformEmail)
			ar.With(canMonitoring).Get("/email-branding", s.admin.GetPlatformEmailBranding)
			ar.With(canMonitoring).Put("/email-branding", s.admin.PutPlatformEmailBranding)
			ar.With(canMonitoring).Post("/email-branding/logo", s.admin.UploadPlatformEmailLogo)
			ar.With(canMonitoring).Get("/notification-deliveries", s.admin.ListAllNotificationDeliveries)
			ar.With(canMonitoring).Post("/notification-deliveries/{id}/retry", s.admin.AdminRetryNotificationDelivery)

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
			 * storefront, menu, opening hours or store status: those belong to
			 * the people who run the business, on their own host.
			 */
			ar.With(canBusinesses).Get("/tenants", s.admin.ListTenants)
			ar.With(canBusinesses).Get("/tenants/slug-available", s.admin.SlugAvailable)
			ar.With(canBusinesses).Get("/tenants/email-available", s.admin.EmailAvailable)
			ar.With(canBusinesses).Post("/tenants", s.admin.CreateTenant)
			ar.With(canBusinesses).Get("/tenants/{id}", s.admin.GetTenant)
			ar.With(canBusinesses).Get("/tenants/{id}/metrics", s.admin.TenantMetrics)
			ar.With(canBusinesses).Get("/tenants/{id}/notification-config", s.admin.GetTenantNotificationConfig)
			ar.With(canBusinesses).Patch("/tenants/{id}", s.admin.UpdateTenant)
			ar.With(canBusinesses).Post("/tenants/{id}/activate", s.admin.ActivateTenant)
			ar.With(canBusinesses).Post("/tenants/{id}/suspend", s.admin.SuspendTenant)
			// Recovering the owner's access is support, not staff management:
			// without it a business whose administrator lost their invitation
			// has no way back in.
			ar.With(canBusinesses).Post("/tenants/{id}/resend-invite", s.admin.ResendTenantInvite)
			ar.With(canBusinesses).Post("/upload", s.admin.UploadAsset)

			// Read-only: the Super Admin seeds a business's initial MFA policy at
			// creation and can see what it currently is, but day-to-day edits
			// belong to that business's own Tenant Admin (PUT /tenant/mfa-policy)
			// the same split already used for the storefront template.
			ar.With(canBusinesses).Get("/tenants/{id}/mfa-policy", s.admin.GetTenantMFAPolicy)

			// A business's own staff are invited and managed day-to-day inside
			// that business, by someone who works there. These three routes are
			// support's read/recover path, not a second place to run a tenant's
			// team from: Support can see who has access and unblock someone who
			// is locked out, exactly as it can resend the owner's own invite
			// above: there is deliberately no route here to create a user or
			// change a name, role or status, the way routine staff management
			// would.
			ar.With(canBusinesses).Get("/tenants/{id}/users", s.admin.ListTenantUsers)
			ar.With(canBusinesses).Post("/tenants/{id}/users/{userId}/reset-access", s.admin.ResetTenantUserAccess)
			ar.With(canBusinesses).Post("/tenants/{id}/users/{userId}/resend-invite", s.admin.ResendTenantUserInvite)
			ar.With(canBusinesses).Post("/tenants/{id}/users/{userId}/reset-mfa", s.admin.ResetUserMFA)

			/* ---------- Console access ----------
			 *
			 * Who can reach this console: the platform/support team, not a
			 * tenant's own staff (that's the block above).
			 */
			ar.With(canConsoleIAM).Get("/users", s.admin.ListConsoleUsers)
			ar.With(canConsoleIAM).Post("/users", s.admin.CreateConsoleUser)
			ar.With(canConsoleIAM).Patch("/users/{id}", s.admin.UpdateConsoleUser)
			ar.With(canConsoleIAM).Post("/users/{id}/resend-invite", s.admin.ResendConsoleInvite)

			/* ---------- Providers ---------- */
			// Dummy vs real payments: platform default and per-business override.
			ar.With(canProviders).Get("/payments/health", s.paymentMode.Health)
			ar.With(canProviders).Post("/payments/{id}/reconcile", s.paymentMode.Reconcile)
			ar.With(canProviders).Get("/payments/settings", s.paymentMode.GetPlatform)
			ar.With(canProviders).Put("/payments/settings", s.paymentMode.PutPlatform)
			ar.With(canProviders).Get("/tenants/{id}/payments", s.paymentMode.GetTenant)
			ar.With(canProviders).Put("/tenants/{id}/payments", s.paymentMode.PutTenant)
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
			tr.Use(auth.MatchHostTenant(s.pool))

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
			// exists for this business type at all: a Hotel tenant's own owner
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
			tr.With(canAnalytics, entitlement.RequireFeature(sqlc.New(s.pool), entitlement.FeatureAnalytics)).Get("/analytics", s.menu.Analytics)
			// What the plan includes and how much of it is used: drives the
			// console's upgrade prompts and license banner.
			tr.Get("/entitlements", entitlement.Handler{Q: sqlc.New(s.pool)}.Get)
			tr.With(canStorefront).Post("/setup/complete-step", s.menu.CompleteSetupStep)
			tr.With(canStorefront).Post("/publish", s.menu.Publish)
			tr.With(canStorefront).Post("/unpublish", s.menu.Unpublish)

			// Activity and audit: the business's own history.
			tr.With(canActivity).Get("/order-history", s.orders.OrderHistory)
			tr.With(canActivity).Get("/activity", s.orders.ActivityFeed)
			tr.With(canActivity).Get("/audit-logs", s.admin.ShopAuditLogs)

			// Access control.
			tr.With(canIAM).Get("/iam", s.admin.ShopIAM)

			// Notifications: every signed-in tenant user sees their own: no
			// extra capability, the router's RequireRoles above is the gate.
			tr.Get("/notifications", s.admin.ListNotifications)
			tr.Post("/notifications/{id}/read", s.admin.MarkNotificationRead)
			tr.Post("/notifications/{id}/unread", s.admin.MarkNotificationUnread)
			tr.Post("/notifications/read-all", s.admin.MarkAllNotificationsRead)
			tr.Get("/me/notification-preferences", s.admin.GetMyNotificationPreferences)
			tr.Put("/me/notification-preferences", s.admin.PutMyNotificationPreferences)

			// Notification settings: this business's own events, rules,
			// templates and delivery history. canOrg: the same permission that
			// already covers "business profile, opening hours, payments and
			// order workflow": notifications are one more facet of how the
			// business is configured.
			tr.With(canOrg).Get("/mfa-policy", s.auth.HandleGetTenantMFAPolicy)
			tr.With(canOrg).Put("/mfa-policy", s.auth.HandlePutTenantMFAPolicy)

			tr.With(canOrg).Get("/notification-events", s.admin.ListTenantNotificationEvents)
			tr.With(canOrg).Get("/notification-rules", s.admin.ListTenantNotificationRules)
			tr.With(canOrg).Put("/notification-rules", s.admin.PutTenantNotificationRule)
			tr.With(canOrg).Delete("/notification-rules", s.admin.DeleteTenantNotificationRule)
			tr.With(canOrg).Get("/notification-templates", s.admin.ListTenantNotificationTemplates)
			tr.With(canOrg).Put("/notification-templates", s.admin.PutTenantNotificationTemplate)
			tr.With(canOrg).Delete("/notification-templates", s.admin.DeleteTenantNotificationTemplate)
			tr.With(canOrg).Post("/notification-templates/test-send", s.admin.TestSendTenantNotificationTemplate)
			tr.With(canOrg).Post("/notification-templates/preview", s.admin.PreviewTenantEmail)
			tr.With(canOrg).Get("/email-branding", s.admin.GetTenantEmailBranding)
			tr.With(canOrg).Put("/email-branding", s.admin.PutTenantEmailBranding)
			tr.With(canOrg).Post("/email-branding/logo", s.admin.UploadTenantEmailLogo)
			tr.With(canOrg).Get("/notification-deliveries", s.admin.ListTenantNotificationDeliveries)
			tr.With(canOrg).Post("/notification-deliveries/{id}/retry", s.admin.RetryTenantNotificationDelivery)

			tr.With(canMenu, reqCatalog).Get("/categories", s.menu.ListCategories)
			tr.With(canMenu, reqCatalog).Post("/categories", s.menu.CreateCategory)
			tr.With(canMenu, reqCatalog).Post("/categories/reorder", s.menu.ReorderCategories)
			tr.With(canMenu, reqCatalog).Patch("/categories/{id}", s.menu.UpdateCategory)
			tr.With(canMenu, reqCatalog).Delete("/categories/{id}", s.menu.DeleteCategory)

			// Inventory: menu permission, catalog capability, and the plan has to
			// include it. A Basic plan gets feature_not_in_plan, not a blank page.
			inv := inventory.NewHandler(s.pool, s.log)
			reqInventory := entitlement.RequireFeature(sqlc.New(s.pool), entitlement.FeatureInventory)
			tr.With(canMenu, reqCatalog, reqInventory).Get("/inventory", inv.List)
			tr.With(canMenu, reqCatalog, reqInventory).Put("/inventory/{productId}", inv.Track)
			tr.With(canMenu, reqCatalog, reqInventory).Post("/inventory/{productId}/adjust", inv.Adjust)
			tr.With(canMenu, reqCatalog, reqInventory).Get("/inventory/{productId}/movements", inv.Movements)

			tr.With(canMenu, reqCatalog).Get("/products", s.menu.ListProducts)
			tr.With(canMenu, reqCatalog).Post("/products", s.menu.CreateProduct)
			tr.With(canMenu, reqCatalog).Post("/products/reorder", s.menu.ReorderProducts)
			tr.With(canMenu, reqCatalog).Post("/products/bulk", s.menu.BulkProducts)
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
			tr.With(canOrg, reqOrders).Post("/payments/{id}/refund", s.paymentMode.Refund)

			// Staff and customers. A manager runs both; staff reach neither.
			// CUSTOMERS/STAFF are "Yes" for every business type in the capability
			// matrix, so these guards never actually block a real tenant today:
			// they exist so that stays true by construction, not by convention.
			tr.With(canStaff, reqStaff).Get("/users", s.admin.ShopListUsers)
			tr.With(canStaff, reqStaff).Post("/users", s.admin.ShopCreateUser)
			tr.With(canStaff, reqStaff).Patch("/users/{userId}", s.admin.ShopUpdateUser)
			tr.With(canStaff, reqStaff).Post("/users/{userId}/reset-access", s.admin.ShopResetUserAccess)
			tr.With(canStaff, reqStaff).Post("/users/{userId}/resend-invite", s.admin.ShopResendUserInvite)
			tr.With(canStaff, reqStaff).Post("/users/{userId}/reset-mfa", s.admin.ShopResetUserMFA)
			tr.With(canStaff, reqStaff).Get("/users/{userId}/logins", s.admin.ShopUserLogins)
			tr.With(canStaff, reqStaff).Get("/users/{userId}/sessions", s.admin.ShopUserSessions)
			tr.With(canStaff, reqStaff).Delete("/users/{userId}/sessions", s.admin.ShopSignOutUser)
			tr.With(canStaff, reqStaff).Get("/users/{userId}/stats", s.admin.ShopUserStats)

			// Attendance: a plan feature the shop also switches on. Every role
			// punches for themselves; the timesheet and settings need "staff".
			{
				att := attendance.NewHandler(s.pool)
				reqAttendance := entitlement.RequireFeature(sqlc.New(s.pool), entitlement.FeatureAttendance)
				tr.With(reqAttendance).Get("/attendance/me", att.Me)
				tr.With(reqAttendance).Post("/attendance/punch-in", att.PunchIn)
				tr.With(reqAttendance).Post("/attendance/punch-out", att.PunchOut)
				tr.With(canStaff, reqStaff, reqAttendance).Get("/attendance", att.List)
				tr.With(canStaff, reqStaff, reqAttendance).Patch("/attendance/{id}", att.Edit)
				tr.With(canStaff, reqStaff, reqAttendance).Get("/attendance/settings", att.GetSettings)
				tr.With(canStaff, reqStaff, reqAttendance).Put("/attendance/settings", att.PutSettings)
			}

			tr.With(canCustomers, reqCustomers).Get("/customers", s.customers.ShopListCustomers)
			tr.With(canCustomers, reqCustomers).Get("/customers/guest", s.customers.ShopGetGuestCustomer)
			tr.With(canCustomers, reqCustomers).Get("/customers/{id}", s.customers.ShopGetCustomer)
			tr.With(canCustomers, reqCustomers).Get("/customers/{id}/activity", s.customers.ShopCustomerActivity)
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
			// The business logo, from Settings. Same handler as menu images,
			// gated on the storefront permission instead of the catalogue
			// capability so a barber or hotel can upload one too.
			tr.With(canStorefront).Post("/branding/upload", s.upload.HandleUpload)
			tr.With(canStorefront).Get("/storefront/preview", s.shop.PreviewMenu)
			tr.With(canStorefront).Get("/storefront/qr", s.shop.QRCode)

			// Marketing: owner-only (storefront permission) and the plan has to
			// include MARKETING. SMS campaigns are additionally checked against
			// NOTIFY_SMS inside the module, at schedule time and again when the
			// worker starts the campaign.
			tr.Group(func(mr chi.Router) {
				mr.Use(canStorefront, entitlement.RequireFeature(sqlc.New(s.pool), entitlement.FeatureMarketing))
				mr.Get("/marketing/overview", s.marketing.Overview)
				mr.Post("/marketing/audience/preview", s.marketing.PreviewAudience)
				mr.Get("/marketing/consent", s.marketing.ConsentSummary)
				mr.Post("/marketing/consent", s.marketing.SetConsent)
				mr.Post("/marketing/consent/grant-all", s.marketing.GrantAll)
				mr.Get("/marketing/links", s.marketing.ListLinks)
				mr.Post("/marketing/links", s.marketing.CreateLink)
				mr.Delete("/marketing/links/{id}", s.marketing.DeleteLink)
				mr.Get("/marketing/links/{id}/qr", s.marketing.LinkQR)
				mr.Get("/marketing/campaigns", s.marketing.ListCampaigns)
				mr.Post("/marketing/campaigns", s.marketing.CreateCampaign)
				mr.Get("/marketing/campaigns/{id}", s.marketing.GetCampaign)
				mr.Patch("/marketing/campaigns/{id}", s.marketing.UpdateCampaign)
				mr.Delete("/marketing/campaigns/{id}", s.marketing.DeleteCampaign)
				mr.Post("/marketing/campaigns/{id}/validate", s.marketing.Validate)
				mr.Post("/marketing/campaigns/{id}/schedule", s.marketing.Schedule)
				mr.Post("/marketing/campaigns/{id}/unschedule", s.marketing.Transition("unschedule"))
				mr.Post("/marketing/campaigns/{id}/pause", s.marketing.Transition("pause"))
				mr.Post("/marketing/campaigns/{id}/resume", s.marketing.Transition("resume"))
				mr.Post("/marketing/campaigns/{id}/cancel", s.marketing.Transition("cancel"))
				mr.Post("/marketing/campaigns/{id}/test-send", s.marketing.TestSend)

				// Offers and loyalty (internal/promo). Redemption and earning
				// happen in the order flow; this is configuration and reporting.
				mr.Get("/marketing/offers", s.promo.ListPromotions)
				mr.Post("/marketing/offers", s.promo.CreatePromotion)
				mr.Patch("/marketing/offers/{id}", s.promo.UpdatePromotion)
				mr.Delete("/marketing/offers/{id}", s.promo.DeletePromotion)
				mr.Post("/marketing/offers/validate", s.promo.ValidatePromotion)
				mr.Get("/marketing/loyalty", s.promo.GetLoyalty)
				mr.Put("/marketing/loyalty", s.promo.PutLoyalty)
				mr.Post("/marketing/loyalty/adjust", s.promo.AdjustLoyalty)

				// Automations: rules the worker evaluates on a schedule.
				mr.Get("/marketing/automations", s.marketing.ListRules)
				mr.Post("/marketing/automations", s.marketing.CreateRule)
				mr.Patch("/marketing/automations/{id}", s.marketing.UpdateRule)
				mr.Delete("/marketing/automations/{id}", s.marketing.DeleteRule)
				mr.Post("/marketing/automations/{id}/enable", s.marketing.SetRuleEnabled(true))
				mr.Post("/marketing/automations/{id}/disable", s.marketing.SetRuleEnabled(false))

				// Social planner: drafts, schedule, publish through Meta.
				mr.Get("/marketing/social", s.marketing.ListPosts)
				mr.Post("/marketing/social", s.marketing.CreatePost)
				mr.Patch("/marketing/social/{id}", s.marketing.UpdatePost)
				mr.Delete("/marketing/social/{id}", s.marketing.DeletePost)
				mr.Post("/marketing/social/{id}/publish", s.marketing.PublishPost)
			})

			// Route aliases for /customize. Shop clients dial the shared API host
			// with X-Tenant-Slug; MatchHostTenant keeps JWT tenant_id aligned.
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
			tr.With(canOrg).Get("/payments/mode", s.paymentMode.GetOwn)
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
			// renders precisely when nobody is signed in: so it cannot read the
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

			pr.With(reqOrders, wrap(s.payLimits.start)).Post("/orders/{orderNumber}/pay", s.payments.Start)
			pr.With(reqOrders, wrap(s.payLimits.confirm)).Post("/orders/{orderNumber}/pay/confirm", s.payments.Confirm)
			pr.With(reqOrders, wrap(s.payLimits.status)).Get("/orders/{orderNumber}/pay", s.payments.Status)

			// Storefront booking: barbers, salons and garages book time;
			// hotels reserve rooms. Each needs the capability that runs it,
			// and a stay needs ROOMS as well as RESERVATIONS because a
			// restaurant has RESERVATIONS (tables) without rooms.
			reqAppointments := tenantctx.RequireCapability(tenantctx.CapAppointments)
			reqRooms := tenantctx.RequireCapability(tenantctx.CapRooms)
			reqReservations := tenantctx.RequireCapability(tenantctx.CapReservations)
			pr.With(reqAppointments).Get("/services", s.appointments.PublicServices)
			pr.With(reqAppointments).Get("/slots", s.appointments.PublicSlots)
			pr.With(reqAppointments, wrap(s.payLimits.book)).Post("/appointments", s.appointments.PublicBook)
			pr.With(reqRooms).Get("/room-types", s.hotel.PublicRoomTypes)
			pr.With(reqRooms, reqReservations, wrap(s.payLimits.book)).Post("/reservations", s.hotel.PublicReserve)
		})

		// Storefront customer identity. Reachable before an order exists, so
		// these routes need no token: the tenant still comes from the host.
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

			reqAppointments := tenantctx.RequireCapability(tenantctx.CapAppointments)
			reqRooms := tenantctx.RequireCapability(tenantctx.CapRooms)
			cr.With(reqAppointments).Get("/appointments", s.appointments.CustomerAppointments)
			cr.With(reqAppointments).Post("/appointments/{id}/cancel", s.appointments.CancelAppointment)
			cr.With(reqAppointments, wrap(s.payLimits.book)).Post("/appointments/{id}/reschedule", s.appointments.RescheduleAppointment)
			cr.With(reqRooms).Get("/reservations", s.hotel.CustomerReservations)
			cr.With(reqRooms).Post("/reservations/{id}/cancel", s.hotel.CancelReservation)
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
		if sub != "" && sub != "www" && sub != "api" {
			return true
		}
	}

	// Production frontends are served over HTTPS with no Vite port:
	// https://orderly.qd.je, https://admin.orderly.qd.je, https://{slug}.orderly.qd.je
	if originIsPublicFrontend(origin, base) {
		return true
	}

	// Local Vite often hops ports (5173/5174/…) when one is busy.
	if s.cfg.AppEnv == "development" || s.cfg.AppEnv == "dev" || s.cfg.AppEnv == "" {
		if originIsLocalDev(origin, base) {
			return true
		}
	}
	return false
}

// originIsPublicFrontend allows browser origins for the deployed frontend:
// apex, admin subdomain, and single-label tenant subdomains on BASE_DOMAIN.
func originIsPublicFrontend(origin, baseDomain string) bool {
	u, err := parseHTTPOrigin(origin)
	if err != nil {
		return false
	}
	// Portful origins are covered by the Vite/local rules above.
	if u.Port() != "" {
		return false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return false
	}
	return hostIsFrontend(strings.ToLower(u.Hostname()), strings.ToLower(baseDomain))
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
	return hostIsFrontend(host, strings.ToLower(baseDomain))
}

func hostIsFrontend(host, base string) bool {
	if host == base || host == "admin."+base {
		return true
	}
	if host == "api."+base || host == "www."+base {
		return false
	}
	suffix := "." + base
	if strings.HasSuffix(host, suffix) {
		sub := strings.TrimSuffix(host, suffix)
		// Single-label tenant slug only (reject nested hosts).
		return sub != "" && sub != "www" && sub != "api" && !strings.Contains(sub, ".")
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

// Version is the build identifier, set at link time:
//
//	go build -ldflags "-X github.com/orderly/orderly-backend/internal/httpserver.Version=<git sha>"
//
// "dev" means a local build.
var Version = "dev"

var processStart = time.Now()

func uptimeSeconds() int64 { return int64(time.Since(processStart).Seconds()) }

// healthDetailAllowed says whether this caller may see build and schema detail.
// With no HEALTH_DETAIL_TOKEN configured (local dev) everyone may; once one is
// set, only a caller presenting it in X-Health-Token. The status itself:
// up or down: is always public, because a monitor needs nothing more.
func (s *Server) healthDetailAllowed(r *http.Request) bool {
	want := s.cfg.HealthDetailToken
	if want == "" {
		return true
	}
	got := r.Header.Get("X-Health-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// handleHealth is liveness: the process is up and serving. It deliberately
// touches nothing else (no database), so a slow database never makes the
// orchestrator kill a healthy API.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"status": "ok"}
	if s.healthDetailAllowed(r) {
		out["version"], out["uptime_seconds"] = Version, uptimeSeconds()
	}
	response.JSON(w, http.StatusOK, out)
}

// handleReady is readiness: the API can actually serve: the database answers,
// and (informationally) which migration it is on, so a deploy that forgot to
// migrate is visible from outside. 503 only when the database is unreachable.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	t0 := time.Now()
	if err := s.pool.Ping(ctx); err != nil {
		s.log.Error("readiness check failed", "error", err)
		response.Error(w, http.StatusServiceUnavailable, "not_ready", "database unavailable")
		return
	}
	db := map[string]any{"ok": true}
	out := map[string]any{"status": "ready", "database": db}
	if s.healthDetailAllowed(r) {
		db["latency_ms"] = time.Since(t0).Milliseconds()
		out["version"], out["uptime_seconds"] = Version, uptimeSeconds()
		// goose records applied migrations here. Best effort: a missing table (a
		// database not managed by goose) just omits the field.
		var schema *int64
		if err := s.pool.QueryRow(ctx,
			`SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&schema); err == nil && schema != nil {
			out["schema_version"] = *schema
		}
	}
	response.JSON(w, http.StatusOK, out)
}

// sandboxSecret is the key the dummy gateway signs its callbacks with.
func sandboxSecret(cfg config.Config) string {
	if cfg.PaymentsSandboxSecret != "" {
		return cfg.PaymentsSandboxSecret
	}
	return cfg.JWTAccessSecret + ":payments-sandbox"
}
