// Package orders is the order workflow: the state machine, its gates and the
// single writer of order status (Handler.Move).
//
// The state machine is part of the frozen V1 commerce/operations contract —
// see orderly/book/LLD.md, and internal/httpserver/workflow_contract_test.go
// which fails if the code, the database CHECK and the contract drift apart.
package orders
