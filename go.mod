// SPDX-License-Identifier: BSD-2-Clause
// SPDX-FileCopyrightText: 2026 Aryan Ameri <github@aryan.ameri.coffee>

module github.com/divbast/librespeed-oci-image

go 1.27.0

require github.com/divbast/ci v0.0.0-00010101000000-000000000000

// The harness and the contract suite, from the checkout of divbast/ci that
// just setup makes at the commit the workflows pin
replace github.com/divbast/ci => ./.ci
