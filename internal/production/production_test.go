package production

import "testing"

func TestIs(t *testing.T) {
	env := func(pairs ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(pairs); i += 2 {
			m[pairs[i]] = pairs[i+1]
		}
		return func(k string) string { return m[k] }
	}
	if !Is(env("PURLVIEW_PLATFORM_ENDPOINT", Endpoint, "PURLVIEW_ENVIRONMENT", Environment)) {
		t.Fatal("the defaults are production")
	}
	for name, get := range map[string]func(string) string{
		"nothing":        env(),
		"staging":        env("PURLVIEW_PLATFORM_ENDPOINT", "https://staging.example.invalid", "PURLVIEW_ENVIRONMENT", "staging"),
		"no environment": env("PURLVIEW_PLATFORM_ENDPOINT", Endpoint),
		"host override":  env("PURLVIEW_PLATFORM_ENDPOINT", Endpoint, "PURLVIEW_ENVIRONMENT", Environment, "PURLVIEW_PLATFORM_HOST", "x.invalid"),
	} {
		if Is(get) {
			t.Errorf("%s counted as production", name)
		}
	}
}
