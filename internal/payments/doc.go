// Package payments owns the payment side of the storefront: the tenant's
// payment method configuration, starting a payment for an order, and applying
// a payment outcome.
//
// Every payment state change happens here and only here, driven by the backend.
// The storefront can ask to pay and can report an outcome, but it can never
// assert that money arrived — the transition is validated against the stored
// payment row, and a captured payment can never be moved back to pending or
// failed.
//
// # Gateway integration
//
// This build ships a sandbox gateway: `Start` mints a single-use intent token
// that the storefront must present to confirm an outcome. That token replaces
// the signature check a real provider webhook would carry, so the same
// endpoints are safe to expose without a live PSP integration. Swapping in a
// real provider means replacing Start/Confirm with the provider's session
// creation and webhook verification — no caller-visible contract changes.
package payments
