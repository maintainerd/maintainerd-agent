package config

import "testing"

// AppVersion is a LINK-TIME constant. The property under test is that no
// environment variable can move it: a version an operator can set is a version
// that can disagree with the binary it describes, and this string is what a
// journal line and Core's fleet view are trusted to answer "what is running"
// with. An unstamped build reports "dev".
func TestAppVersionIsNotEnvironmentConfigurable(t *testing.T) {
	before := AppVersion
	for _, key := range []string{"APP_VERSION", "AGENT_VERSION", "VERSION"} {
		t.Setenv(key, "9.9.9-attacker")
	}
	Load()
	if AppVersion != before {
		t.Fatalf("AppVersion = %q after Load with version env vars set, want %q (link-time only)", AppVersion, before)
	}
	if AppVersion == "" {
		t.Fatal("AppVersion is empty — Register and the boot line would report no version at all")
	}
}
