package viperconfig

import (
	"os"
	"testing"
)

// withConfig writes a config.yaml into a temp dir and makes it the process CWD
// for the test, which is where Load's default search path looks first.
func withConfig(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/config.yaml", []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if _, ok := os.LookupEnv("GO_ENV"); ok {
		t.Setenv("GO_ENV", "")
		os.Unsetenv("GO_ENV")
	}
}

type sliceConfig struct {
	Name      string   `mapstructure:"name" optional:"true"`
	Endpoints []string `mapstructure:"endpoints" optional:"true"`
	Nested    struct {
		URLs []string `mapstructure:"urls" optional:"true"`
	} `mapstructure:"nested"`
}

// The regression this release exists for: a []string field whose only source is
// an env var, with no YAML key to hang it on. Before the env-key binding it
// arrived nil, whatever the key was named.
func TestCSV_EnvOnlySlice(t *testing.T) {
	withConfig(t, "name: fromyaml\n")
	t.Setenv("APP_ENDPOINTS", "a:1,b:2")
	cfg, err := Load(&sliceConfig{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Endpoints) != 2 || cfg.Endpoints[0] != "a:1" || cfg.Endpoints[1] != "b:2" {
		t.Fatalf("got %#v, want [a:1 b:2]", cfg.Endpoints)
	}
}

func TestCSV_EnvOnlyNestedSlice(t *testing.T) {
	withConfig(t, "name: fromyaml\n")
	t.Setenv("APP_NESTED_URLS", " a:1 , b:2 ")
	cfg, err := Load(&sliceConfig{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Nested.URLs) != 2 || cfg.Nested.URLs[0] != "a:1" {
		t.Fatalf("got %#v, want [a:1 b:2] trimmed", cfg.Nested.URLs)
	}
}

// A YAML-declared key overridden by a CSV env var: the v1.3.0 path, unchanged.
func TestCSV_YAMLSeededSliceEnvOverride(t *testing.T) {
	withConfig(t, "name: fromyaml\nendpoints: [\"old:0\"]\n")
	t.Setenv("APP_ENDPOINTS", "a:1,b:2")
	cfg, err := Load(&sliceConfig{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Endpoints) != 2 {
		t.Fatalf("got %#v, want the env list", cfg.Endpoints)
	}
}

// A YAML list stays a list: nothing to split, nothing to break.
func TestCSV_YAMLListUntouched(t *testing.T) {
	withConfig(t, "name: n\nendpoints: [\"a:1\", \"b:2\"]\n")
	cfg, err := Load(&sliceConfig{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Endpoints) != 2 {
		t.Fatalf("got %#v, want 2 entries", cfg.Endpoints)
	}
}

func TestCSV_SingleValueIsAOneElementList(t *testing.T) {
	withConfig(t, "name: n\n")
	t.Setenv("APP_ENDPOINTS", "only:1")
	cfg, err := Load(&sliceConfig{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Endpoints) != 1 || cfg.Endpoints[0] != "only:1" {
		t.Fatalf("got %#v, want [only:1]", cfg.Endpoints)
	}
}

func TestCSV_UnsetStaysEmpty(t *testing.T) {
	withConfig(t, "name: n\n")
	os.Unsetenv("APP_ENDPOINTS")
	cfg, err := Load(&sliceConfig{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Endpoints) != 0 {
		t.Fatalf("got %#v, want empty", cfg.Endpoints)
	}
}

type requiredConfig struct {
	Host    string `mapstructure:"host"`
	EnvOnly string `mapstructure:"envOnly"`
}

// Quirk (a): before binding, a required field with no YAML key was rejected
// however loudly the environment set it.
func TestRequired_EnvOnlyFieldIsAccepted(t *testing.T) {
	withConfig(t, "host: h\n")
	t.Setenv("APP_ENVONLY", "iamset")
	cfg, err := Load(&requiredConfig{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.EnvOnly != "iamset" {
		t.Fatalf("got %q, want iamset", cfg.EnvOnly)
	}
}

func TestRequired_StillFailsWhenTrulyMissing(t *testing.T) {
	withConfig(t, "host: h\n")
	os.Unsetenv("APP_ENVONLY")
	if _, err := Load(&requiredConfig{}); err == nil {
		t.Fatal("want an error for a genuinely missing required field")
	}
}

func TestWithoutEnvKeyBinding_RestoresV13Behaviour(t *testing.T) {
	withConfig(t, "host: h\n")
	t.Setenv("APP_ENVONLY", "iamset")
	if _, err := Load(&requiredConfig{}, WithoutEnvKeyBinding()); err == nil {
		t.Fatal("want the pre-1.4 'required is missing' error with binding off")
	}
}

type defaultsConfig struct {
	Reject   bool   `mapstructure:"reject" optional:"true" default:"true"`
	Attempts int    `mapstructure:"attempts" optional:"true" default:"3"`
	URI      string `mapstructure:"uri" optional:"true" default:"mongodb://localhost:27017"`
}

// Quirks (b) and (c) as they still behave by default: a `default` tag is
// re-applied over an explicitly configured zero value.
func TestDefaults_LenientIsStillTheDefault(t *testing.T) {
	withConfig(t, "reject: false\nattempts: 0\nuri: \"\"\n")
	cfg, err := Load(&defaultsConfig{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Reject || cfg.Attempts != 3 || cfg.URI == "" {
		t.Fatalf("v1.x behaviour changed: %+v", cfg)
	}
}

func TestDefaults_StrictHonoursExplicitZeroValues(t *testing.T) {
	withConfig(t, "reject: false\nattempts: 0\nuri: \"\"\n")
	cfg, err := Load(&defaultsConfig{}, WithStrictDefaults())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Reject {
		t.Error("reject: explicit false was overwritten by default:\"true\"")
	}
	if cfg.Attempts != 0 {
		t.Errorf("attempts: got %d, want the explicit 0", cfg.Attempts)
	}
	if cfg.URI != "" {
		t.Errorf("uri: got %q, want the explicit empty string", cfg.URI)
	}
}

func TestDefaults_StrictFromEnv(t *testing.T) {
	withConfig(t, "reject: true\n")
	t.Setenv("APP_REJECT", "false")
	cfg, err := Load(&defaultsConfig{}, WithStrictDefaults())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Reject {
		t.Error("an env var setting false must win over default:\"true\"")
	}
}

// Strict mode only steps aside for values that are actually configured: an
// absent key still gets its default.
func TestDefaults_StrictStillAppliesWhenUnset(t *testing.T) {
	withConfig(t, "name: n\n")
	os.Unsetenv("APP_REJECT")
	os.Unsetenv("APP_ATTEMPTS")
	os.Unsetenv("APP_URI")
	cfg, err := Load(&defaultsConfig{}, WithStrictDefaults())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Reject || cfg.Attempts != 3 || cfg.URI == "" {
		t.Fatalf("defaults did not apply to unset keys: %+v", cfg)
	}
}

type stringURLConfig struct {
	URL  string   `mapstructure:"url" optional:"true"`
	URLs []string `mapstructure:"urls" optional:"true"`
}

// The fleet encodes fallback lists as a CSV inside a singular `url` STRING
// field (ZERO_<SVC>_URL) and splits it itself. Splitting is driven by the
// destination type, so those fields must come through with the commas intact —
// a slice landing in a string field would fail the unmarshal outright.
func TestCSV_StringFieldKeepsItsCommas(t *testing.T) {
	withConfig(t, "url: \"\"\n")
	t.Setenv("APP_URL", "svc.pr-ns:4107,svc.dev:4107")
	cfg, err := Load(&stringURLConfig{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.URL != "svc.pr-ns:4107,svc.dev:4107" {
		t.Fatalf("got %q, want the raw CSV", cfg.URL)
	}
}

// Same key name the pre-1.4 converter special-cased, now reached through the
// env alone.
func TestCSV_EnvOnlyUrlsKey(t *testing.T) {
	withConfig(t, "url: \"\"\n")
	t.Setenv("APP_URLS", "a:1,b:2")
	cfg, err := Load(&stringURLConfig{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.URLs) != 2 {
		t.Fatalf("got %#v, want 2 entries", cfg.URLs)
	}
}
