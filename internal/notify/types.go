// Package notify publishes in-app notifications — a persistent, per-recipient
// feed a signed-in user can open, distinct from the audit log (write-only,
// no reader) and from email (configsvc/SMTP, invites only).
package notify

// Event types. A closed set, like this codebase's other small enums
// (storefront.Workflow's acceptance_mode etc.) — TEXT column, Go constants,
// not a Postgres ENUM type. Every value here has a matching row in the
// notification_events catalog (db/migrations/20260929240000_notification_rules_engine.sql)
// — that row is what the rules engine resolves against; this constant is
// just a typo-proof way for Go code to reference it.
const (
	TypeOrderPlaced       = "ORDER_PLACED"
	TypeOrderReady        = "ORDER_READY"
	TypeOrderCancelled    = "ORDER_CANCELLED"
	TypeBusinessOnboarded = "BUSINESS_ONBOARDED"

	TypePaymentFailed       = "PAYMENT_FAILED"
	TypeSubscriptionChanged = "SUBSCRIPTION_CHANGED"

	TypeAppointmentCreated   = "APPOINTMENT_CREATED"
	TypeAppointmentConfirmed = "APPOINTMENT_CONFIRMED"
	TypeAppointmentCancelled = "APPOINTMENT_CANCELLED"
	TypeAppointmentNoShow    = "APPOINTMENT_NO_SHOW"

	TypeReservationCreated    = "RESERVATION_CREATED"
	TypeReservationConfirmed  = "RESERVATION_CONFIRMED"
	TypeReservationCancelled  = "RESERVATION_CANCELLED"
	TypeReservationNoShow     = "RESERVATION_NO_SHOW"
	TypeReservationCheckedIn  = "RESERVATION_CHECKED_IN"
	TypeReservationCheckedOut = "RESERVATION_CHECKED_OUT"

	TypeStaffInvited     = "STAFF_INVITED"
	TypeStaffRoleChanged = "STAFF_ROLE_CHANGED"
	TypeStaffDisabled    = "STAFF_DISABLED"
	TypeStaffEnabled     = "STAFF_ENABLED"

	TypeSecurityLoginFailed     = "SECURITY_LOGIN_FAILED"
	TypeSecurityMFAEnabled      = "SECURITY_MFA_ENABLED"
	TypeSecurityMFADisabled     = "SECURITY_MFA_DISABLED"
	TypeSecurityMFAMethodAdded  = "SECURITY_MFA_METHOD_ADDED"
	TypeSecurityMFARecoveryUsed = "SECURITY_MFA_RECOVERY_USED"
	TypeSecurityMFAAdminReset   = "SECURITY_MFA_ADMIN_RESET"
	TypeSecurityMFAPolicyChanged = "SECURITY_MFA_POLICY_CHANGED"
	TypeSecurityPasswordReset   = "SECURITY_PASSWORD_RESET"
	TypeSecurityPasswordChanged = "SECURITY_PASSWORD_CHANGED"

	TypePlatformSystemError       = "PLATFORM_SYSTEM_ERROR"
	TypePlatformOperationalAlert  = "PLATFORM_OPERATIONAL_ALERT"
)

// Channels a rule can target.
const (
	ChannelInApp = "IN_APP"
	ChannelEmail = "EMAIL"
	ChannelSMS   = "SMS"
)

// Recipient policies a rule can carry. "ROLE:" is followed by an
// identity.Permission value, e.g. "ROLE:selling".
const (
	PolicyPlatform     = "PLATFORM"
	PolicyCustomer     = "CUSTOMER"
	rolePolicyPrefix   = "ROLE:"
)
