// Package production names the platform an installed CLI and its daemon use
// when no platform is configured.
package production

// The production platform: its environment name and account API. The edge
// a share docks on comes with the share from the platform.
const (
	Environment = "production"
	Endpoint    = "https://purlview.dev"
)

// Is reports whether the resolved settings name the production platform, as
// the defaults do. Its daemon files keep the plain paths people are pointed
// to; any other platform gets a directory of its own beside them.
func Is(getenv func(string) string) bool {
	return getenv("PURLVIEW_PLATFORM_ENDPOINT") == Endpoint &&
		getenv("PURLVIEW_ENVIRONMENT") == Environment &&
		getenv("PURLVIEW_PLATFORM_HOST") == ""
}
