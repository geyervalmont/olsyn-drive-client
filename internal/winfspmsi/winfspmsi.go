// Package winfspmsi carries the unmodified WinFsp installer (MSI) inside the
// Windows program, so one downloaded file is all a person needs. Builds made
// with the build tag winfspmsi embed it (make fetches the pinned release
// from the WinFsp project into build/ and checks it against SHA256); other
// builds carry nothing and Bytes returns nil.
//
// WinFsp is (c) Bill Zissimopoulos and is used under the WinFsp FLOSS
// exception (see NOTICE). The file is the publisher's, byte for byte.
package winfspmsi

const (
	// Version of the embedded installer.
	Version = "2.2.26323"
	// File is the release asset name.
	File = "winfsp-" + Version + ".msi"
	// ReleaseTag is the WinFsp project's release the installer comes from;
	// SourceURL is that release's complete source code.
	ReleaseTag = "v2.2"
	SourceURL  = "https://github.com/winfsp/winfsp/tree/" + ReleaseTag
	// SHA256 pins the release asset (checked when it is fetched, and again
	// before it is run elevated).
	SHA256 = "36ce01f9eedbbe6842b7fb4bedc79d7d4f71983e8aceb6877b46a07b331c7e19"
	// Size of the release asset in bytes.
	Size = 2220032
	// MinVersion is the oldest installed WinFsp (DLL file version) the
	// client works with: an installed WinFsp at least this new is used as
	// it is and the installer is not run.
	MinVersion = "2.0"
)

// Bytes returns the embedded installer, or nil in a build without it.
func Bytes() []byte { return embedded }
