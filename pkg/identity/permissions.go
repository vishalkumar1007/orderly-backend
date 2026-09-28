package identity

import "sort"

/*
Role permissions.

The product defines three business roles — owner, manager, staff — and says
access is permission-based rather than "manager gets everything the owner has".
This file is the single place that decides what each role may reach; the HTTP
layer asks it, and the IAM screen renders it, so the answer a user is shown and
the answer the API enforces can never disagree.

Custom per-tenant roles are deliberately out of scope for now: the product
defers them, and a fixed table with one owner is honest about what is enforced.
When they arrive, this map becomes the seed for the roles table rather than
something to unpick.
*/

// Permission is one capability inside a business.
type Permission string

const (
	// Running: the daily operation of the shop.
	PermSelling Permission = "selling"
	PermKitchen Permission = "kitchen"
	PermLive    Permission = "live_activity"

	// Management: the things a shop sells and the people it serves.
	PermMenu      Permission = "menu"
	PermCustomers Permission = "customers"
	PermStaff     Permission = "staff"

	// Configuration: how the business itself is set up.
	PermStorefront   Permission = "storefront"
	PermOrganization Permission = "organization"
	PermIntegrations Permission = "integrations"
	PermIAM          Permission = "iam"
	PermSettings     Permission = "settings"

	// Insight: what the business has been doing.
	PermAnalytics Permission = "analytics"
	PermActivity  Permission = "activity"
)

/* ------------------------------------------------------------------ *
 * Platform capabilities
 *
 * The console is not a single-seat product. The owner can bring in people to
 * run it with them, and the only safe way to do that is to say exactly what
 * each of them may reach — sharing the owner's password is the alternative
 * these roles exist to remove.
 * ------------------------------------------------------------------ */

const (
	// PermPlatformBusinesses covers onboarding and managing businesses.
	PermPlatformBusinesses Permission = "platform_businesses"
	// PermPlatformPlans covers the plan catalogue and subscriptions.
	PermPlatformPlans Permission = "platform_plans"
	// PermPlatformProviders covers platform credentials and who may use them.
	PermPlatformProviders Permission = "platform_providers"
	// PermPlatformIAM covers who can reach this console.
	PermPlatformIAM Permission = "platform_iam"
	// PermPlatformSettings covers platform settings and branding.
	PermPlatformSettings Permission = "platform_settings"
	// PermPlatformMonitoring covers activity, audit and system health.
	PermPlatformMonitoring Permission = "platform_monitoring"
)

// platformPermissions lists the console capabilities in the order IAM shows
// them.
var platformPermissions = []PermissionInfo{
	{PermPlatformBusinesses, "Businesses", "Platform", "Onboard businesses, edit their details, suspend and restore them"},
	{PermPlatformPlans, "Plans & subscriptions", "Platform", "Create and withdraw plans, and move a business between them"},
	{PermPlatformProviders, "Providers & integrations", "Platform", "Configure platform email, storage and AI, and who may use them"},
	{PermPlatformSettings, "Platform settings", "Platform", "Platform identity, branding, security policy and business types"},
	{PermPlatformIAM, "Console access", "Platform", "Invite people to this console and change what they can reach"},
	{PermPlatformMonitoring, "Monitoring", "Platform", "Activity, the audit log and system health"},
}

// PlatformPermissions returns the console capability catalogue.
func PlatformPermissions() []PermissionInfo { return platformPermissions }

// platformRolePermissions decides what each console role may reach.
//
// Support is read-shaped on purpose: it exists so somebody can answer "what is
// happening with this business" without being able to change the answer. The
// read/write split is enforced per route, not here — this decides which screens
// are reachable at all.
var platformRolePermissions = map[string]map[Permission]bool{
	RoleSupport: set(
		PermPlatformBusinesses, PermPlatformMonitoring,
	),
	RolePlatformAdmin: set(
		PermPlatformBusinesses, PermPlatformPlans, PermPlatformProviders,
		PermPlatformSettings, PermPlatformMonitoring,
	),
	RoleSuperAdmin: set(
		PermPlatformBusinesses, PermPlatformPlans, PermPlatformProviders,
		PermPlatformSettings, PermPlatformIAM, PermPlatformMonitoring,
	),
}

// PlatformRoles returns the console roles, most access first.
func PlatformRoles() []RoleInfo {
	return []RoleInfo{
		{
			Key:         RoleSuperAdmin,
			Label:       "Owner",
			Description: "Full control of the platform, including who else can reach this console. Exactly one account holds it and it cannot be removed.",
			Permissions: PermissionsFor(RoleSuperAdmin),
			Assignable:  false,
		},
		{
			Key:         RolePlatformAdmin,
			Label:       "Platform admin",
			Description: "Runs the platform alongside the owner: businesses, plans, providers and settings. Cannot change who has console access.",
			Permissions: PermissionsFor(RolePlatformAdmin),
			Assignable:  true,
		},
		{
			Key:         RoleSupport,
			Label:       "Support",
			Description: "Can see businesses and monitoring to answer questions. Changes nothing.",
			Permissions: PermissionsFor(RoleSupport),
			Assignable:  true,
		},
	}
}

// IsPlatformRole reports whether a role belongs to the console rather than to a
// business.
func IsPlatformRole(role string) bool {
	switch role {
	case RoleSuperAdmin, RolePlatformAdmin, RoleSupport:
		return true
	}
	return false
}

// IsAssignablePlatformRole reports whether a role can be granted to someone.
// The owner is deliberately excluded: there is exactly one, by construction.
func IsAssignablePlatformRole(role string) bool {
	switch role {
	case RolePlatformAdmin, RoleSupport:
		return true
	}
	return false
}

// PermissionInfo describes a permission for the IAM screen.
type PermissionInfo struct {
	Key         Permission `json:"key"`
	Label       string     `json:"label"`
	Group       string     `json:"group"`
	Description string     `json:"description"`
}

// allPermissions is the catalogue, in the order the IAM screen lists it.
var allPermissions = []PermissionInfo{
	{PermSelling, "Selling", "Running", "Take orders, accept them and move them through the board"},
	{PermKitchen, "Kitchen", "Running", "Work the kitchen board and mark orders ready"},
	{PermLive, "Live activity", "Running", "Open the customer-facing pickup display"},

	{PermMenu, "Menu", "Management", "Add and edit categories, products, options and prices"},
	{PermCustomers, "Customers", "Management", "See customer records and order history"},
	{PermStaff, "Staff", "Management", "Invite people and manage who works here"},

	{PermStorefront, "Storefront", "Configuration", "Branding, theme, homepage and what customers see"},
	{PermOrganization, "Organization", "Configuration", "Business profile, opening hours, payments and order workflow"},
	{PermIntegrations, "Integrations", "Configuration", "Email, storage and AI configuration for this business"},
	{PermIAM, "Access control", "Configuration", "Change what other people in this business may reach"},
	{PermSettings, "Settings", "Configuration", "Organization-level settings"},

	{PermAnalytics, "Analytics", "Insight", "Dashboard figures, trends and best sellers"},
	{PermActivity, "Activity & audit", "Insight", "Order history, activity feed and the audit log"},
}

// Permissions returns the catalogue.
func Permissions() []PermissionInfo { return allPermissions }

// rolePermissions is the authority on what each role may do.
//
// Staff get the two running screens and nothing else — they take orders, they
// do not reprice the menu. A manager runs the shop day to day, including the
// menu, customers and staff, but not the things that change the business
// itself: storefront, payments, order workflow, integrations, access control.
// Only the owner changes those, because those are the settings a manager is
// measured against.
var rolePermissions = map[string]map[Permission]bool{
	RoleStaff: set(
		PermSelling, PermKitchen,
	),
	RoleManager: set(
		PermSelling, PermKitchen, PermLive,
		PermMenu, PermCustomers, PermStaff,
		PermAnalytics, PermActivity,
	),
	RoleTenantAdmin: set(
		PermSelling, PermKitchen, PermLive,
		PermMenu, PermCustomers, PermStaff,
		PermStorefront, PermOrganization, PermIntegrations, PermIAM, PermSettings,
		PermAnalytics, PermActivity,
	),
}

// RoleInfo describes a business role for the IAM screen.
type RoleInfo struct {
	Key         string       `json:"key"`
	Label       string       `json:"label"`
	Description string       `json:"description"`
	Permissions []Permission `json:"permissions"`
	/// Assignable reports whether this role can be given to someone here. The
	/// platform owner is never assignable inside a business.
	Assignable bool `json:"assignable"`
}

// BusinessRoles returns the roles a business can assign, most access first.
func BusinessRoles() []RoleInfo {
	return []RoleInfo{
		{
			Key:         RoleTenantAdmin,
			Label:       "Owner",
			Description: "Full control of this business, including configuration and access.",
			Permissions: PermissionsFor(RoleTenantAdmin),
			Assignable:  true,
		},
		{
			Key:         RoleManager,
			Label:       "Manager",
			Description: "Runs the shop day to day: orders, kitchen, menu, customers and staff. Cannot change how the business is configured.",
			Permissions: PermissionsFor(RoleManager),
			Assignable:  true,
		},
		{
			Key:         RoleStaff,
			Label:       "Staff",
			Description: "Takes orders and works the kitchen board. Sees no configuration.",
			Permissions: PermissionsFor(RoleStaff),
			Assignable:  true,
		},
	}
}

// Can reports whether a role holds a permission.
//
// Business and platform roles are looked up in separate tables and never fall
// through to one another. That is the boundary: a platform role holds no
// business capability, so a console token cannot satisfy a tenant route even if
// one were ever mounted without its guards, and the reverse holds too.
func Can(role string, permission Permission) bool {
	if IsPlatformRole(role) {
		return platformRolePermissions[role][permission]
	}
	return rolePermissions[role][permission]
}

// PermissionsFor returns a role's permissions in catalogue order, so two calls
// never render the same role in a different order.
func PermissionsFor(role string) []Permission {
	catalogue := allPermissions
	held := rolePermissions[role]
	if IsPlatformRole(role) {
		catalogue = platformPermissions
		held = platformRolePermissions[role]
	}
	out := make([]Permission, 0, len(held))
	for _, info := range catalogue {
		if held[info.Key] {
			out = append(out, info.Key)
		}
	}
	return out
}

// IsBusinessRole reports whether a role can be assigned inside a business.
func IsBusinessRole(role string) bool {
	switch role {
	case RoleTenantAdmin, RoleManager, RoleStaff:
		return true
	}
	return false
}

// RoleLabel is the human name for a role.
func RoleLabel(role string) string {
	switch role {
	case RoleSuperAdmin:
		return "Owner"
	case RolePlatformAdmin:
		return "Platform admin"
	case RoleSupport:
		return "Support"
	case RoleTenantAdmin:
		return "Owner"
	case RoleManager:
		return "Manager"
	case RoleStaff:
		return "Staff"
	}
	return role
}

// RoleRank orders roles by how much access they hold, most first. Used for
// stable sorting in listings.
func RoleRank(role string) int {
	switch role {
	case RoleSuperAdmin:
		return -3
	case RolePlatformAdmin:
		return -2
	case RoleSupport:
		return -1
	case RoleTenantAdmin:
		return 0
	case RoleManager:
		return 1
	case RoleStaff:
		return 2
	}
	return 3
}

// SortRoles orders a slice of roles by access, most first.
func SortRoles(roles []string) {
	sort.SliceStable(roles, func(i, j int) bool {
		return RoleRank(roles[i]) < RoleRank(roles[j])
	})
}

func set(permissions ...Permission) map[Permission]bool {
	out := make(map[Permission]bool, len(permissions))
	for _, p := range permissions {
		out[p] = true
	}
	return out
}
