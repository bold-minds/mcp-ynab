// SPDX-License-Identifier: MIT
package main

// Canonical UUIDs for test fixtures.
//
// Every tool handler runs its identifier arguments through
// validateUUIDOrLookup (see tools.go), which rejects non-UUID strings at
// the boundary. Tests have to supply real UUIDs to reach the handler
// body. Rather than sprinkle hand-rolled UUID literals across every
// test file, they reference named constants from here.
//
// Conventions:
//   - Each category of ID has a distinct high byte so a test failure's
//     UUID tells you which kind was wrong at a glance.
//   - The "second" variants (e.g. testPlanIDAlt) exist for tests that
//     need two distinct ids of the same kind.
//   - All ids are v4-style (random) but validateUUIDOrLookup does NOT
//     check version bits, so the values below are arbitrary — the only
//     constraint is canonical 8-4-4-4-12 hex shape.
//
// Do not reuse these constants outside _test.go files. They are
// intentionally package-private to the test suite.
const (
	testPlanID    = "11111111-1111-4111-8111-111111111111"
	testPlanIDAlt = "12121212-1212-4121-8121-121212121212"

	testAccountID     = "22222222-2222-4222-8222-222222222222"
	testAccountIDAlt  = "23232323-2323-4232-8232-232323232323"
	testAccountIDAlt2 = "24242424-2424-4242-8242-242424242424"

	testCategoryID     = "33333333-3333-4333-8333-333333333333"
	testCategoryIDAlt  = "34343434-3434-4343-8343-343434343434"
	testCategoryIDAlt2 = "35353535-3535-4353-8353-353535353535"

	testPayeeID    = "44444444-4444-4444-8444-444444444444"
	testPayeeIDAlt = "45454545-4545-4454-8454-454545454545"

	testTransactionID    = "55555555-5555-4555-8555-555555555555"
	testTransactionIDAlt = "56565656-5656-4565-8565-565656565656"
)
