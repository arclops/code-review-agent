package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func boolPtr(value bool) *bool { return &value }

func writeConfigFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing the config file: %v", err)
	}
	return path
}

func TestDefaultIsUsableWithNothingButAToken(t *testing.T) {
	config := Default()

	if err := config.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want the defaults to be valid", err)
	}
	if config.Model.Provider != "none" {
		t.Errorf("Model.Provider = %q, want %q", config.Model.Provider, "none")
	}
	if config.Model.Name != "" {
		t.Errorf("Model.Name = %q, want it empty for the none provider", config.Model.Name)
	}
	if config.Model.MaxTokens != 1500 {
		t.Errorf("Model.MaxTokens = %d, want 1500", config.Model.MaxTokens)
	}
	if config.Model.Timeout.Duration() != 60*time.Second {
		t.Errorf("Model.Timeout = %v, want 60s", config.Model.Timeout.Duration())
	}
	if !config.SecretsEnabled() {
		t.Error("SecretsEnabled() = false, want the secret scan on by default")
	}
	if config.TokenEnv != "GITHUB_TOKEN" {
		t.Errorf("TokenEnv = %q, want GITHUB_TOKEN", config.TokenEnv)
	}
	if config.Notify.URLEnv != "REVIEW_NOTIFY_URL" {
		t.Errorf("Notify.URLEnv = %q, want REVIEW_NOTIFY_URL", config.Notify.URLEnv)
	}
	if config.Limits.MaxConcurrentRuns != 4 || config.Limits.PerFileConcurrenc != 4 {
		t.Errorf("Limits = %+v, want the concurrency limits to be 4", config.Limits)
	}
	if config.Limits.Retries != 3 || config.Limits.RetryBaseDelay.Duration() != 200*time.Millisecond {
		t.Errorf("Limits = %+v, want 3 retries with a 200ms base delay", config.Limits)
	}
	if !config.Checkout.Enabled || config.Checkout.Depth != 1 {
		t.Errorf("Checkout = %+v, want it enabled at depth 1", config.Checkout)
	}
	if len(config.Guidelines) == 0 {
		t.Error("Guidelines is empty, want the default guidelines")
	}
	if !strings.Contains(strings.Join(config.Guidelines, "\n"), "Never commit secrets") {
		t.Errorf("Guidelines = %v, want the default rules", config.Guidelines)
	}
	for _, pattern := range []string{"vendor/*", "node_modules/*", "*.pb.go"} {
		found := false
		for _, skip := range config.Filters.SkipPatterns {
			if skip == pattern {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("SkipPatterns is missing %q", pattern)
		}
	}
}

func TestLoadWithNoPathReturnsTheDefaults(t *testing.T) {
	config, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") = %v, want the defaults", err)
	}
	if !reflect.DeepEqual(config, Default()) {
		t.Errorf("Load(\"\") = %+v, want exactly Default()", config)
	}
}

func TestLoadOverridesOnlyTheFieldsTheFileMentions(t *testing.T) {
	path := writeConfigFile(t, `{
		"model": {"provider": "openai", "name": "gpt-4o-mini", "base_url": "https://example.test/v1"},
		"limits": {"retries": 7, "run_timeout_seconds": 90},
		"lint": {"commands": [{"name": "ruff", "command": ["ruff", "check"], "format": "ruff-json", "stream": "stderr"}]},
		"api_url": "https://github.enterprise.test/api/v3"
	}`)

	config, err := Load(path)
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	if config.Model.Provider != "openai" || config.Model.Name != "gpt-4o-mini" {
		t.Errorf("Model = %+v, want the provider and name from the file", config.Model)
	}
	if config.Model.BaseURL != "https://example.test/v1" {
		t.Errorf("Model.BaseURL = %q, want the value from the file", config.Model.BaseURL)
	}
	// Fields absent from the file keep their defaults.
	if config.Model.MaxTokens != 1500 {
		t.Errorf("Model.MaxTokens = %d, want the default 1500", config.Model.MaxTokens)
	}
	if config.Model.Timeout.Duration() != 60*time.Second {
		t.Errorf("Model.Timeout = %v, want the default 60s", config.Model.Timeout.Duration())
	}
	if config.Model.APIKeyEnv != "OPENAI_API_KEY" {
		t.Errorf("Model.APIKeyEnv = %q, want the default OPENAI_API_KEY", config.Model.APIKeyEnv)
	}
	if config.Limits.Retries != 7 {
		t.Errorf("Limits.Retries = %d, want 7", config.Limits.Retries)
	}
	if config.Limits.RunTimeout.Duration() != 90*time.Second {
		t.Errorf("Limits.RunTimeout = %v, want 90s", config.Limits.RunTimeout.Duration())
	}
	if config.Limits.MaxConcurrentRuns != 4 {
		t.Errorf("Limits.MaxConcurrentRuns = %d, want the default 4", config.Limits.MaxConcurrentRuns)
	}
	if config.Limits.RetryBaseDelay.Duration() != 200*time.Millisecond {
		t.Errorf("Limits.RetryBaseDelay = %v, want the default 200ms", config.Limits.RetryBaseDelay.Duration())
	}
	if len(config.Lint.Commands) != 1 || config.Lint.Commands[0].Format != "ruff-json" {
		t.Errorf("Lint.Commands = %+v, want the command from the file", config.Lint.Commands)
	}
	if config.Lint.Commands[0].Stream != StreamStderr {
		t.Errorf("Lint.Commands[0].Stream = %q, want %q", config.Lint.Commands[0].Stream, StreamStderr)
	}
	if config.APIURL != "https://github.enterprise.test/api/v3" {
		t.Errorf("APIURL = %q, want the value from the file", config.APIURL)
	}
	if !config.SecretsEnabled() {
		t.Error("SecretsEnabled() = false, want the default true to survive")
	}
	if !reflect.DeepEqual(config.Guidelines, DefaultGuidelines) {
		t.Error("Guidelines changed, want the defaults to survive a file that does not mention them")
	}
	if config.TokenEnv != "GITHUB_TOKEN" || config.Notify.URLEnv != "REVIEW_NOTIFY_URL" {
		t.Errorf("Config = %+v, want the environment names to keep their defaults", config)
	}
	if err := config.Validate(); err != nil {
		t.Errorf("Validate() = %v, want the loaded configuration to be valid", err)
	}
}

func TestLoadRejectsAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")
	config, err := Load(path)
	if err == nil {
		t.Fatal("Load() = nil, want an error for a missing file")
	}
	if !strings.Contains(err.Error(), "could not read") {
		t.Errorf("error = %q, want it to say the file could not be read", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name %q", err, path)
	}
	if !reflect.DeepEqual(config, Config{}) {
		t.Errorf("Load() = %+v, want a zero configuration on error", config)
	}
}

func TestLoadRejectsInvalidJSON(t *testing.T) {
	cases := map[string]string{
		"truncated object": `{"model": {"provider": "openai"`,
		"wrong type":       `{"limits": {"max_concurrent_runs": "many"}}`,
		"not json at all":  `provider: openai`,
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeConfigFile(t, contents)
			config, err := Load(path)
			if err == nil {
				t.Fatal("Load() = nil, want an error for invalid JSON")
			}
			if !strings.Contains(err.Error(), "is not valid JSON") {
				t.Errorf("error = %q, want it to say the file is not valid JSON", err)
			}
			if !reflect.DeepEqual(config, Config{}) {
				t.Errorf("Load() = %+v, want a zero configuration on error", config)
			}
		})
	}
}

func TestSecondsUnmarshalsIntegersFloatsAndRejectsNonsense(t *testing.T) {
	cases := []struct {
		name       string
		input      string
		want       time.Duration
		wantErrSub string
	}{
		{name: "whole seconds", input: "12", want: 12 * time.Second},
		{name: "a float of seconds", input: "2.5", want: 2500 * time.Millisecond},
		{name: "negative seconds", input: "-3", want: -3 * time.Second},
		{name: "null is zero", input: "null", want: 0},
		{name: "a string is not a number", input: `"30"`, wantErrSub: "expected a number of seconds"},
		{name: "a bare word is not JSON at all", input: "soon", wantErrSub: "invalid character"},
		{name: "an object is not a number", input: `{"seconds":1}`, wantErrSub: "expected a number of seconds"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var seconds Seconds
			err := json.Unmarshal([]byte(testCase.input), &seconds)
			if testCase.wantErrSub != "" {
				if err == nil {
					t.Fatalf("Unmarshal(%s) = nil, want an error", testCase.input)
				}
				if !strings.Contains(err.Error(), testCase.wantErrSub) {
					t.Errorf("error = %q, want it to contain %q", err, testCase.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%s) = %v, want nil", testCase.input, err)
			}
			if seconds.Duration() != testCase.want {
				t.Errorf("Unmarshal(%s) = %v, want %v", testCase.input, seconds.Duration(), testCase.want)
			}
		})
	}
}

func TestSecondsMarshalsWholeSeconds(t *testing.T) {
	cases := []struct {
		seconds Seconds
		want    string
	}{
		{seconds: Seconds(90 * time.Second), want: "90"},
		{seconds: Seconds(0), want: "0"},
		{seconds: Seconds(5 * time.Minute), want: "300"},
		// A fraction of a second must survive the round trip: the default retry
		// delay is 200ms, and truncating it to zero seconds would silently
		// remove the backoff.
		{seconds: Seconds(2500 * time.Millisecond), want: "2.5"},
		{seconds: Seconds(200 * time.Millisecond), want: "0.2"},
	}
	for _, testCase := range cases {
		got, err := json.Marshal(testCase.seconds)
		if err != nil {
			t.Fatalf("Marshal(%v) = %v, want nil", testCase.seconds, err)
		}
		if string(got) != testCase.want {
			t.Errorf("Marshal(%v) = %s, want %s", testCase.seconds, got, testCase.want)
		}
	}
}

func TestSecondsRoundTripsInsideAConfiguration(t *testing.T) {
	config := Default()
	config.Limits.RunTimeout = Seconds(90 * time.Second)

	encoded, err := json.Marshal(config.Limits)
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil", err)
	}
	if !strings.Contains(string(encoded), `"run_timeout_seconds":90`) {
		t.Errorf("encoded limits = %s, want run_timeout_seconds to be 90", encoded)
	}
	if !strings.Contains(string(encoded), `"call_timeout_seconds":60`) {
		t.Errorf("encoded limits = %s, want call_timeout_seconds to be 60", encoded)
	}

	var decoded LimitsConfig
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() = %v, want nil", err)
	}
	if decoded.RunTimeout.Duration() != 90*time.Second {
		t.Errorf("RunTimeout = %v, want 90s after a round trip", decoded.RunTimeout.Duration())
	}
}

func TestApplyEnvReadsTheConfiguredTokenVariable(t *testing.T) {
	environment := map[string]string{
		"GITHUB_TOKEN":   "default-token",
		"MY_TOKEN":       "custom-token",
		"GITHUB_API_URL": "https://github.enterprise.test/api/v3",
	}
	lookup := func(name string) string { return environment[name] }

	t.Run("configured name", func(t *testing.T) {
		config := Default()
		config.TokenEnv = "MY_TOKEN"
		config.ApplyEnv(lookup)
		if config.GithubToken != "custom-token" {
			t.Errorf("GithubToken = %q, want the value of MY_TOKEN", config.GithubToken)
		}
		if config.APIURL != "https://github.enterprise.test/api/v3" {
			t.Errorf("APIURL = %q, want the value of GITHUB_API_URL", config.APIURL)
		}
	})

	t.Run("empty name falls back to GITHUB_TOKEN", func(t *testing.T) {
		config := Default()
		config.TokenEnv = ""
		config.ApplyEnv(lookup)
		if config.TokenEnv != "GITHUB_TOKEN" {
			t.Errorf("TokenEnv = %q, want the fallback", config.TokenEnv)
		}
		if config.GithubToken != "default-token" {
			t.Errorf("GithubToken = %q, want the value of GITHUB_TOKEN", config.GithubToken)
		}
	})

	t.Run("missing variables leave the values alone", func(t *testing.T) {
		config := Default()
		config.TokenEnv = "ABSENT"
		config.APIURL = "https://configured.example.test/api/v3"
		config.ApplyEnv(func(string) string { return "" })
		if config.GithubToken != "" {
			t.Errorf("GithubToken = %q, want it empty", config.GithubToken)
		}
		if config.APIURL != "https://configured.example.test/api/v3" {
			t.Errorf("APIURL = %q, want a configured value to survive an unset variable", config.APIURL)
		}
	})

	t.Run("the environment overrides the configured API URL", func(t *testing.T) {
		config := Default()
		config.APIURL = "https://configured.example.test/api/v3"
		config.ApplyEnv(lookup)
		if config.APIURL != "https://github.enterprise.test/api/v3" {
			t.Errorf("APIURL = %q, want the environment to win", config.APIURL)
		}
	})

	t.Run("the lookup sees the token variable and the API URL variable", func(t *testing.T) {
		var asked []string
		config := Default()
		config.TokenEnv = "MY_TOKEN"
		config.ApplyEnv(func(name string) string {
			asked = append(asked, name)
			return environment[name]
		})
		want := []string{"MY_TOKEN", "GITHUB_API_URL"}
		if len(asked) != len(want) {
			t.Fatalf("looked up %v, want %v", asked, want)
		}
		for index := range want {
			if asked[index] != want[index] {
				t.Errorf("looked up %v, want %v", asked, want)
				break
			}
		}
	})
}

func TestSecretsEnabledDefaultsOnWhenThePointerIsNil(t *testing.T) {
	cases := []struct {
		name string
		set  *bool
		want bool
	}{
		{name: "nil means on", set: nil, want: true},
		{name: "explicitly on", set: boolPtr(true), want: true},
		{name: "explicitly off", set: boolPtr(false), want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := Default()
			config.Lint.SecretScan = testCase.set
			if got := config.SecretsEnabled(); got != testCase.want {
				t.Errorf("SecretsEnabled() = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestSeverityForPrefersTheLongestMatchingPrefix(t *testing.T) {
	config := Default()
	config.Lint.Severity = map[string]string{
		"S":      SeverityMinor,
		"SEC":    SeverityMajor,
		"SECRET": SeverityCritical,
	}

	cases := []struct {
		name string
		rule string
		want string
	}{
		{name: "the longest prefix wins", rule: "SECRET_KEY", want: SeverityCritical},
		{name: "a shorter prefix when it is the only match", rule: "SEC123", want: SeverityMajor},
		{name: "the shortest prefix as a last resort", rule: "S1", want: SeverityMinor},
		{name: "matching ignores case", rule: "secret_token", want: SeverityCritical},
		{name: "an unknown rule falls back to minor", rule: "XYZ", want: SeverityMinor},
		{name: "an empty rule falls back to minor", rule: "", want: SeverityMinor},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := config.SeverityFor(testCase.rule); got != testCase.want {
				t.Errorf("SeverityFor(%q) = %q, want %q", testCase.rule, got, testCase.want)
			}
		})
	}
}

func TestSeverityForUsesTheDefaultRuleTable(t *testing.T) {
	config := Default()
	cases := []struct {
		rule string
		want string
	}{
		{rule: "SECRET", want: SeverityCritical},
		{rule: "S105", want: SeverityCritical},
		{rule: "F401", want: SeverityMajor},
		{rule: "E501", want: SeverityMinor},
		{rule: "W291", want: SeverityMinor},
		{rule: "C901", want: SeverityMinor},
	}
	for _, testCase := range cases {
		t.Run(testCase.rule, func(t *testing.T) {
			if got := config.SeverityFor(testCase.rule); got != testCase.want {
				t.Errorf("SeverityFor(%q) = %q, want %q", testCase.rule, got, testCase.want)
			}
		})
	}
}

func TestSeverityForFallsBackToMinorWithoutAnyRuleTable(t *testing.T) {
	config := Default()
	config.Lint.Severity = nil
	if got := config.SeverityFor("SECRET"); got != SeverityMinor {
		t.Errorf("SeverityFor(SECRET) = %q, want %q with no rule table", got, SeverityMinor)
	}
}

func TestSeverityOrderRanksTheWorstFirst(t *testing.T) {
	if SeverityOrder[SeverityCritical] >= SeverityOrder[SeverityMajor] {
		t.Errorf("SeverityOrder = %v, want CRITICAL to rank before MAJOR", SeverityOrder)
	}
	if SeverityOrder[SeverityMajor] >= SeverityOrder[SeverityMinor] {
		t.Errorf("SeverityOrder = %v, want MAJOR to rank before MINOR", SeverityOrder)
	}
}

func TestValidateAcceptsEveryProviderItDocuments(t *testing.T) {
	cases := []struct {
		provider string
		name     string
	}{
		{provider: "", name: ""},
		{provider: "none", name: ""},
		{provider: "openai", name: "gpt-4o-mini"},
		{provider: "anthropic", name: "claude-3-5-sonnet"},
		{provider: "ollama", name: "llama3"},
	}
	for _, testCase := range cases {
		t.Run("provider_"+testCase.provider, func(t *testing.T) {
			config := Default()
			config.Model.Provider = testCase.provider
			config.Model.Name = testCase.name
			if err := config.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil for provider %q", err, testCase.provider)
			}
		})
	}
}

func TestValidateRejectsConfigurationsThatCannotWork(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "unknown provider",
			mutate:  func(c *Config) { c.Model.Provider = "gemini" },
			wantErr: `unknown model provider "gemini"`,
		},
		{
			name: "a provider with no model name",
			mutate: func(c *Config) {
				c.Model.Provider = "openai"
				c.Model.Name = ""
			},
			wantErr: `model provider "openai" needs a model name`,
		},
		{
			name: "an anthropic provider with no model name",
			mutate: func(c *Config) {
				c.Model.Provider = "anthropic"
				c.Model.Name = ""
			},
			wantErr: `model provider "anthropic" needs a model name`,
		},
		{
			name:    "zero concurrent runs",
			mutate:  func(c *Config) { c.Limits.MaxConcurrentRuns = 0 },
			wantErr: "max_concurrent_runs must be at least 1",
		},
		{
			name:    "negative concurrent runs",
			mutate:  func(c *Config) { c.Limits.MaxConcurrentRuns = -2 },
			wantErr: "max_concurrent_runs must be at least 1",
		},
		{
			name:    "zero per file concurrency",
			mutate:  func(c *Config) { c.Limits.PerFileConcurrenc = 0 },
			wantErr: "per_file_concurrency must be at least 1",
		},
		{
			name:    "negative per file concurrency",
			mutate:  func(c *Config) { c.Limits.PerFileConcurrenc = -1 },
			wantErr: "per_file_concurrency must be at least 1",
		},
		{
			name:    "zero retries",
			mutate:  func(c *Config) { c.Limits.Retries = 0 },
			wantErr: "retries must be at least 1",
		},
		{
			name:    "negative retries",
			mutate:  func(c *Config) { c.Limits.Retries = -3 },
			wantErr: "retries must be at least 1",
		},
		{
			name: "a lint command with nothing to run",
			mutate: func(c *Config) {
				c.Lint.Commands = []LintCommand{{Name: "ruff", Format: "ruff-json"}}
			},
			wantErr: `lint command "ruff" has nothing to run`,
		},
		{
			name: "a lint command with an empty argument list",
			mutate: func(c *Config) {
				c.Lint.Commands = []LintCommand{{Name: "eslint", Command: []string{}, Format: "eslint-json"}}
			},
			wantErr: `lint command "eslint" has nothing to run`,
		},
		{
			name: "a lint command with an unknown format",
			mutate: func(c *Config) {
				c.Lint.Commands = []LintCommand{{Name: "semgrep", Command: []string{"semgrep"}, Format: "sarif"}}
			},
			wantErr: `lint command "semgrep" has unknown output format "sarif"`,
		},
		{
			name: "a lint command with no format at all",
			mutate: func(c *Config) {
				c.Lint.Commands = []LintCommand{{Name: "custom", Command: []string{"custom"}}}
			},
			wantErr: `lint command "custom" has unknown output format ""`,
		},
		{
			name: "a lint command with an unknown stream",
			mutate: func(c *Config) {
				c.Lint.Commands = []LintCommand{{Name: "vet", Command: []string{"go", "vet"}, Format: "go-vet", Stream: "somewhere"}}
			},
			wantErr: `lint command "vet" has unknown stream "somewhere"`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := Default()
			testCase.mutate(&config)
			err := config.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", testCase.wantErr)
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, testCase.wantErr)
			}
		})
	}
}

func TestValidateAcceptsEveryKnownLintFormat(t *testing.T) {
	for format := range Parsers {
		t.Run(format, func(t *testing.T) {
			config := Default()
			config.Lint.Commands = []LintCommand{{Name: "tool", Command: []string{"tool", "run"}, Format: format}}
			if err := config.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil for the %q parser", err, format)
			}
		})
	}
}

func TestValidateAcceptsTheDocumentedStreams(t *testing.T) {
	for _, stream := range []string{"", StreamStdout, StreamStderr, StreamBoth} {
		name := stream
		if name == "" {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			config := Default()
			config.Lint.Commands = []LintCommand{{Name: "vet", Command: []string{"go", "vet"}, Format: "go-vet", Stream: stream}}
			if err := config.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil for stream %q", err, stream)
			}
		})
	}
}

func TestStreamConstantsAreTheDocumentedNames(t *testing.T) {
	if StreamStdout != "stdout" || StreamStderr != "stderr" || StreamBoth != "both" {
		t.Errorf("streams = %q, %q, %q, want stdout, stderr, both", StreamStdout, StreamStderr, StreamBoth)
	}
}

func TestParsersOnlyListsTheFormatsTheCodeUnderstands(t *testing.T) {
	for _, format := range []string{"ruff-json", "eslint-json", "go-vet", "gofmt-list", "plain"} {
		if !Parsers[format] {
			t.Errorf("Parsers[%q] = false, want the documented parser", format)
		}
	}
	if Parsers["sarif"] {
		t.Error("Parsers[sarif] = true, want an unknown format to be rejected")
	}
}
