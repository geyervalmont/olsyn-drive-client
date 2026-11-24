//go:build winfspmsi

package winfspmsi

import _ "embed"

// embedded is the WinFsp MSI, copied here by make (gitignored).
//
//go:embed winfsp.msi
var embedded []byte
