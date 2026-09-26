// Package storefront owns everything the public customer storefront renders:
// store identity, the controlled design-token theme, the configuration-driven
// homepage, payment method availability, order workflow rules and opening
// hours.
//
// Two audiences share this package. Public handlers expose a strictly filtered
// projection of the configuration (never tenant-private settings, never
// payment secrets). Tenant admin handlers read and write the same rows behind
// the tenant admin role, and the tenant is always resolved from the request
// context — never from a client-supplied id.
package storefront
