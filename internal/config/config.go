// Package config holds everything the review agent can be told, and the
// defaults it falls back on.
//
// The guidelines are part of the configuration rather than part of the prompt
// for a reason the challenge makes well: the people who want to change the
// rules are usually not the people who are comfortable editing the code that
// builds the prompt.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Severity levels, worst first. The order is the order findings are posted in.
const (
	SeverityCritical = "CRITICAL"
	SeverityMajor    = "MAJOR"
	SeverityMinor    = "MINOR"
)

// SeverityOrder ranks the levels so that the worst is at the top of the comment
// rather than four paragraphs down.
var SeverityOrder = map[string]int{
	SeverityCritical: 0,
	SeverityMajor:    1,
	SeverityMinor:    2,
}

// DefaultGuidelines are used when no guidelines file is given. They are the
// rules a review agent can actually check, in the order they matter.
var DefaultGuidelines = []string{
	"Never commit secrets, credentials, API keys or tokens.",
	"Match the intent described in the pull request: flag code that does something else.",
	"Guard against division by zero and other arithmetic on values that can be zero.",
	"Prefer early returns over deeply nested conditionals.",
	"Give every function and module a short comment saying what it is for.",
	"Name things for what they mean, not for how they are implemented.",
}

// ModelConfig describes the language model to use. Provider "none" runs the
// deterministic checks only, which is a supported way to run: it needs no
// credentials and no network.
type ModelConfig struct {
	Provider  string  `json:"provider"`
	BaseURL   string  `json:"base_url"`
	Name      string  `json:"name"`
	APIKeyEnv string  `json:"api_key_env"`
	Timeout   Seconds `json:"timeout_seconds"`
	MaxTokens int     `json:"max_output_tokens"`
	Models    []Model `json:"-"`
}

// Model is one entry of a chain of models to try in order.
type Model struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	Name     string `json:"name"`
}

// Streams a lint command can write its findings to. Most tools use stdout, but
// the Go toolchain writes diagnostics to stderr, so a runner that only read
// stdout would report nothing for go vet and look like a clean run.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
	StreamBoth   = "both"
)

// LintCommand is one static analysis tool to run over the checked out code.
type LintCommand struct {
	Name    string   `json:"name"`
	Command []string `json:"command"`
	Format  string   `json:"format"`
	Stream  string   `json:"stream"`
}

// LintConfig describes the deterministic checks.
type LintConfig struct {
	Commands   []LintCommand     `json:"commands"`
	SecretScan *bool             `json:"secret_scan"`
	Severity   map[string]string `json:"severity"`
}

// FiltersConfig decides which of a pull request's files are worth reviewing.
type FiltersConfig struct {
	SkipPatterns   []string `json:"skip_patterns"`
	MaxFiles       int      `json:"max_files"`
	MaxPatchBytes  int      `json:"max_patch_bytes"`
	IncludeDeleted bool     `json:"include_deleted"`
}

// LimitsConfig bounds the work.
type LimitsConfig struct {
	MaxConcurrentRuns int     `json:"max_concurrent_runs"`
	PerFileConcurrenc int     `json:"per_file_concurrency"`
	RunTimeout        Seconds `json:"run_timeout_seconds"`
	CallTimeout       Seconds `json:"call_timeout_seconds"`
	Retries           int     `json:"retries"`
	RetryBaseDelay    Seconds `json:"retry_base_delay_seconds"`
}

// CheckoutConfig says whether to clone the repository so that a linter can run
// over the whole file rather than just the diff.
type CheckoutConfig struct {
	Enabled bool `json:"enabled"`
	Depth   int  `json:"depth"`

	// GitConfig is extra git configuration for the fetch, as "key=value"
	// entries. A proxy, a private certificate authority or a TLS backend that
	// does not work on the host all need it.
	GitConfig []string `json:"git_config"`
}

// NotifyConfig is where a failed run is reported. Both Slack and Discord accept
// one of the keys in the payload, so one setting works for either.
type NotifyConfig struct {
	URLEnv  string  `json:"url_env"`
	Timeout Seconds `json:"timeout_seconds"`
}

// Config is the whole configuration.
type Config struct {
	Guidelines []string       `json:"guidelines"`
	Model      ModelConfig    `json:"model"`
	Lint       LintConfig     `json:"lint"`
	Filters    FiltersConfig  `json:"filters"`
	Limits     LimitsConfig   `json:"limits"`
	Checkout   CheckoutConfig `json:"checkout"`
	Notify     NotifyConfig   `json:"notify"`

	// GithubToken is read from the environment and never written down.
	GithubToken string `json:"-"`

	// APIURL overrides the GitHub API endpoint. GitHub Enterprise Server serves
	// the same API on another host, so this is a deployment setting rather than
	// a testing one.
	APIURL   string `json:"api_url"`
	TokenEnv string `json:"token_env"`
}

// Seconds is a duration written as a number of seconds in JSON.
type Seconds time.Duration

// MarshalJSON writes the duration back as seconds, keeping the fraction so that
// a value like 200ms survives a round trip instead of becoming zero.
func (s Seconds) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(s).Seconds())
}

// UnmarshalJSON reads a number of seconds.
func (s *Seconds) UnmarshalJSON(data []byte) error {
	var seconds float64
	if err := json.Unmarshal(data, &seconds); err != nil {
		return fmt.Errorf("expected a number of seconds: %w", err)
	}
	*s = Seconds(time.Duration(seconds * float64(time.Second)))
	return nil
}

// Duration is the value as a time.Duration.
func (s Seconds) Duration() time.Duration { return time.Duration(s) }

// Default returns a configuration that works with nothing but a GitHub token:
// the deterministic checks, no model, no checkout.
func Default() Config {
	return Config{
		Guidelines: DefaultGuidelines,
		Model: ModelConfig{
			Provider:  "none",
			APIKeyEnv: "OPENAI_API_KEY",
			Timeout:   Seconds(60 * time.Second),
			MaxTokens: 1500,
		},
		Lint: LintConfig{
			SecretScan: boolPointer(true),
			Severity: map[string]string{
				"SECRET": SeverityCritical,
				"S":      SeverityCritical,
				"F":      SeverityMajor,
				"E":      SeverityMinor,
				"W":      SeverityMinor,
			},
		},
		Filters: FiltersConfig{
			SkipPatterns: []string{
				"*.lock", "*.lock.json", "*.sum", "package-lock.json", "yarn.lock", "uv.lock",
				"vendor/*", "node_modules/*", "dist/*", "build/*", "third_party/*",
				"*.min.js", "*.min.css", "*.pb.go", "*_generated.go", "*.snap",
			},
			MaxFiles:      200,
			MaxPatchBytes: 40000,
		},
		Limits: LimitsConfig{
			MaxConcurrentRuns: 4,
			PerFileConcurrenc: 4,
			RunTimeout:        Seconds(5 * time.Minute),
			CallTimeout:       Seconds(60 * time.Second),
			Retries:           3,
			RetryBaseDelay:    Seconds(200 * time.Millisecond),
		},
		Checkout: CheckoutConfig{Enabled: true, Depth: 1},
		Notify:   NotifyConfig{URLEnv: "REVIEW_NOTIFY_URL", Timeout: Seconds(10 * time.Second)},
		TokenEnv: "GITHUB_TOKEN",
	}
}

// Load reads a configuration file over the defaults. An empty path returns the
// defaults, so the service runs with no configuration file at all.
func Load(path string) (Config, error) {
	config := Default()
	if path == "" {
		return config, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: could not read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, fmt.Errorf("config: %s is not valid JSON: %w", path, err)
	}
	return config, nil
}

// ApplyEnv fills in the values that belong in the environment: the tokens, and
// anything a deployment wants to override without a new config file.
func (c *Config) ApplyEnv(lookup func(string) string) {
	if c.TokenEnv == "" {
		c.TokenEnv = "GITHUB_TOKEN"
	}
	c.GithubToken = lookup(c.TokenEnv)
	// GitHub Actions sets GITHUB_API_URL on every runner, including the
	// Enterprise Server ones where it is the only way to find the API.
	if url := lookup("GITHUB_API_URL"); url != "" {
		c.APIURL = url
	}
}

// SecretsEnabled reports whether the built in secret scan runs. It is on by
// default, because the one finding a reviewer should never miss is a committed
// credential.
func (c Config) SecretsEnabled() bool {
	return c.Lint.SecretScan == nil || *c.Lint.SecretScan
}

// SeverityFor maps a rule to a severity, by the longest matching prefix so that
// more specific rules win.
func (c Config) SeverityFor(rule string) string {
	best := ""
	severity := ""
	for prefix, level := range c.Lint.Severity {
		if strings.HasPrefix(strings.ToUpper(rule), strings.ToUpper(prefix)) && len(prefix) > len(best) {
			best = prefix
			severity = level
		}
	}
	if severity == "" {
		return SeverityMinor
	}
	return severity
}

// Validate reports configuration that cannot work, before anything runs.
func (c Config) Validate() error {
	switch c.Model.Provider {
	case "", "none", "openai", "anthropic", "ollama":
	default:
		return fmt.Errorf("config: unknown model provider %q", c.Model.Provider)
	}
	if c.Model.Provider != "" && c.Model.Provider != "none" && c.Model.Name == "" {
		return fmt.Errorf("config: model provider %q needs a model name", c.Model.Provider)
	}
	if c.Limits.MaxConcurrentRuns < 1 {
		return fmt.Errorf("config: max_concurrent_runs must be at least 1")
	}
	if c.Limits.PerFileConcurrenc < 1 {
		return fmt.Errorf("config: per_file_concurrency must be at least 1")
	}
	if c.Limits.Retries < 1 {
		return fmt.Errorf("config: retries must be at least 1")
	}
	for _, command := range c.Lint.Commands {
		if len(command.Command) == 0 {
			return fmt.Errorf("config: lint command %q has nothing to run", command.Name)
		}
		if _, ok := Parsers[command.Format]; !ok {
			return fmt.Errorf("config: lint command %q has unknown output format %q", command.Name, command.Format)
		}
		switch command.Stream {
		case "", StreamStdout, StreamStderr, StreamBoth:
		default:
			return fmt.Errorf("config: lint command %q has unknown stream %q, want stdout, stderr or both",
				command.Name, command.Stream)
		}
	}
	return nil
}

// Parsers names the linter output formats that are understood.
var Parsers = map[string]bool{
	"ruff-json":   true,
	"eslint-json": true,
	"go-vet":      true,
	"gofmt-list":  true,
	"plain":       true,
}

func boolPointer(value bool) *bool { return &value }
