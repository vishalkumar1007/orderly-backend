// Package customers implements storefront customer identity: phone-number
// login via one-time codes, the profile endpoint and order history.
//
// A customer is identified by (tenant, phone). Logging in is always optional —
// the storefront must let anyone order as a guest — so nothing in the guest
// checkout path depends on this package succeeding.
package customers
