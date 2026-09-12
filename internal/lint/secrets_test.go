package lint

import (
	"reflect"
	"strings"
	"testing"

	"github.com/arclops/code-review-agent/internal/diff"
)

// The samples are one obviously synthetic value per default rule, shaped like
// the provider's key so that the pattern matches, and assembled from pieces at
// run time rather than written out whole.
//
// That last part is not fussiness. A repository that scans for credentials must
// not itself contain a string shaped like one: GitHub's push protection refuses
// the push, and it is right to. The scanner sees exactly the same string either
// way.
var (
	fakeAWSKeyID    = "AKIA" + "IOSFODNN7EXAMPLE"
	fakeAWSSecret   = "wJalrXUtnFEMI/K7MDENG/" + "bPxRfiCYEXAMPLEKEY"
	fakeGitHubToken = "ghp_" + "A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	fakeStripeKey   = "sk_" + "live_" + "51H8xYzAbCdEfGhIjKl"
	fakeSlackToken  = "xoxb" + "-123456789012-abcdefghijkl0123"
	fakeGoogleKey   = "AIza" + "SyD4kQ9xLm2Nv7Rb1Tc5Wd8Ye3Zf6Uh0JgK"
	fakePrivateKey  = "-----BEGIN " + "RSA PRIVATE KEY-----"
	fakeCredential  = "hunter2-Sup3rS3cret"
)

// fakeSecrets is one sample per default rule.
var fakeSecrets = []struct {
	rule   string
	secret string
	line   string
}{
	{
		rule:   "aws-access-key-id",
		secret: fakeAWSKeyID,
		line:   `aws_access_key_id = "` + fakeAWSKeyID + `"`,
	},
	{
		rule:   "aws-secret-access-key",
		secret: fakeAWSSecret,
		line:   `aws_secret_access_key = "` + fakeAWSSecret + `"`,
	},
	{
		rule:   "github-token",
		secret: fakeGitHubToken,
		line:   "GITHUB_TOKEN=" + fakeGitHubToken,
	},
	{
		rule:   "stripe-secret-key",
		secret: fakeStripeKey,
		line:   `STRIPE_KEY = "` + fakeStripeKey + `"`,
	},
	{
		rule:   "slack-token",
		secret: fakeSlackToken,
		line:   "SLACK_TOKEN=" + fakeSlackToken,
	},
	{
		rule:   "google-api-key",
		secret: fakeGoogleKey,
		line:   `GOOGLE_API_KEY = "` + fakeGoogleKey + `"`,
	},
	{
		rule:   "private-key",
		secret: fakePrivateKey,
		line:   fakePrivateKey,
	},
	{
		rule:   "hardcoded-credential",
		secret: fakeCredential,
		line:   `password = "` + fakeCredential + `"`,
	},
}

func TestEveryDefaultSecretRuleFiresOnItsOwnFakeCredential(t *testing.T) {
	covered := make(map[string]bool)
	for _, sample := range fakeSecrets {
		covered[sample.rule] = true
		t.Run(sample.rule, func(t *testing.T) {
			findings := ScanLines("app/config.py", []diff.Line{{Number: 7, Text: sample.line, Added: true}})
			if len(findings) != 1 {
				t.Fatalf("finding = %#v from %s, want exactly one for %s", findings, sample.line, sample.rule)
			}
			got := findings[0]
			if got.Rule != sample.rule {
				t.Errorf("Rule = %q, want %q", got.Rule, sample.rule)
			}
			if got.Severity != "CRITICAL" {
				t.Errorf("Severity = %q, want %q", got.Severity, "CRITICAL")
			}
			if got.File != "app/config.py" {
				t.Errorf("File = %q, want %q", got.File, "app/config.py")
			}
			if got.Line != 7 {
				t.Errorf("Line = %d, want 7", got.Line)
			}
			if got.Tool != "secret-scan" {
				t.Errorf("Tool = %q, want %q", got.Tool, "secret-scan")
			}
			wantMessage := "a value matching " + sample.rule + " was added here; move it to a secret store and rotate it"
			if got.Message != wantMessage {
				t.Errorf("Message = %q, want %q", got.Message, wantMessage)
			}
			// A review comment is public; it must not repeat the credential.
			if strings.Contains(got.Message, sample.secret) {
				t.Errorf("Message %q contains the secret it reports", got.Message)
			}
		})
	}

	for _, rule := range DefaultSecretRules {
		if !covered[rule.Name] {
			t.Errorf("rule %q has no sample line in this table", rule.Name)
		}
	}
}

func TestTheFindingMessageNeverQuotesTheMatchedLine(t *testing.T) {
	secret := fakeStripeKey
	line := `const key = "` + secret + `"`

	findings := ScanLines("web/config.js", []diff.Line{{Number: 3, Text: line, Added: true}})
	if len(findings) == 0 {
		t.Fatalf("no findings for %s, want the stripe key reported", line)
	}
	for _, finding := range findings {
		if strings.Contains(finding.Message, secret) || strings.Contains(finding.Message, line) {
			t.Errorf("finding %q repeats the credential it reports", finding.Message)
		}
	}
}

func TestPlaceholderValuesAreNotReportedAsSecrets(t *testing.T) {
	tests := []struct {
		name string
		line string
	}{
		{name: "changeme", line: `password = "changeme"`},
		{name: "xxxx", line: `api_key = "xxxxxxxxxxxx"`},
		{name: "example", line: `secret = "example-value"`},
		{name: "YOUR_KEY_HERE", line: `access_token = "YOUR_ACCESS_TOKEN"`},
		{name: "placeholder", line: `auth_token = "placeholder-value"`},
		{name: "dummy", line: `password = "dummy-pass"`},
		{name: "redacted", line: `secret = "redacted-value"`},
		{name: "sample", line: `secret = "sample-secret-value"`},
		{name: "an angle bracket stand-in", line: `secret = "<your-secret-here>"`},
		{name: "a template expansion", line: `api_key = "${API_KEY}"`},
		{name: "ellipses", line: `password = "................"`},
		{name: "a TODO", line: `secret = "TODO-fill-this-in"`},
		{name: "an unquoted process.env reference", line: `password: process.env.DB_PASSWORD`},
		{name: "a value too short to be a secret", line: `password = "***"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			findings := ScanLines("app/.env.example", []diff.Line{{Number: 1, Text: test.line, Added: true}})
			if len(findings) != 0 {
				t.Errorf("findings = %#v for %s, want none", findings, test.line)
			}
		})
	}
}

func TestProviderKeyFormatsAreReportedEvenWhenTheValueSaysExample(t *testing.T) {
	// The documented trade off: a value shaped like a provider key is worth
	// reporting whatever it says, because that is how a leaked key looks.
	line := `AWS_ACCESS_KEY_ID=` + fakeAWSKeyID

	findings := ScanLines("app/deploy.sh", []diff.Line{{Number: 1, Text: line, Added: true}})
	if len(findings) != 1 || findings[0].Rule != "aws-access-key-id" {
		t.Errorf("findings = %#v, want one aws-access-key-id finding", findings)
	}
}

func TestScanPatchReportsOnlyTheLinesThePatchAdds(t *testing.T) {
	patch := `diff --git a/app/config.py b/app/config.py
index 4f3a1c2..9b2e7d4 100644
--- a/app/config.py
+++ b/app/config.py
@@ -1,4 +1,4 @@
 import os
-PASSWORD = "old-secret-hunter2"
+API_TOKEN = "` + fakeGitHubToken + `"
 context_key = "` + fakeAWSKeyID + `"
`

	findings := ScanPatch("app/config.py", patch)
	if len(findings) != 1 {
		t.Fatalf("findings = %#v, want only the added line reported", findings)
	}
	got := findings[0]
	if got.Rule != "github-token" {
		t.Errorf("Rule = %q, want %q", got.Rule, "github-token")
	}
	if got.Line != 2 {
		t.Errorf("Line = %d, want 2 (the line the patch adds)", got.Line)
	}
	if got.File != "app/config.py" {
		t.Errorf("File = %q, want %q", got.File, "app/config.py")
	}
}

func TestScanPatchWithNoAddedLinesReportsNothing(t *testing.T) {
	tests := []struct {
		name  string
		patch string
	}{
		{
			name: "only context and removed lines",
			patch: `diff --git a/app/config.py b/app/config.py
--- a/app/config.py
+++ b/app/config.py
@@ -1,3 +1,2 @@
 import os
-PASSWORD = "` + fakeCredential + `"
 KEY = "` + fakeAWSKeyID + `"
`,
		},
		{name: "an empty patch", patch: ""},
		{name: "a patch that only touches a binary file", patch: "diff --git a/logo.png b/logo.png\nBinary files a/logo.png and b/logo.png differ\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if findings := ScanPatch("app/config.py", test.patch); len(findings) != 0 {
				t.Errorf("findings = %#v, want none", findings)
			}
		})
	}
}

func TestALineThatMatchesSeveralRulesYieldsAFindingForEach(t *testing.T) {
	line := `creds := "` + fakeAWSKeyID + `" + "` + fakeStripeKey + `"`

	findings := ScanLines("app/creds.go", []diff.Line{{Number: 12, Text: line, Added: true}})
	var rules []string
	for _, finding := range findings {
		rules = append(rules, finding.Rule)
		if finding.Line != 12 {
			t.Errorf("Line = %d, want 12", finding.Line)
		}
	}
	want := []string{"aws-access-key-id", "stripe-secret-key"}
	if !reflect.DeepEqual(rules, want) {
		t.Errorf("rules = %q, want %q", rules, want)
	}
}

func TestDefaultSecretRulesAreWellFormed(t *testing.T) {
	if len(DefaultSecretRules) == 0 {
		t.Fatal("no default secret rules")
	}
	seen := make(map[string]bool)
	for _, rule := range DefaultSecretRules {
		if rule.Name == "" {
			t.Error("a rule has no name")
		}
		if seen[rule.Name] {
			t.Errorf("rule %q is listed twice", rule.Name)
		}
		seen[rule.Name] = true
		if rule.Pattern == nil {
			t.Errorf("rule %q has no pattern", rule.Name)
		}
		if rule.Severity != "CRITICAL" {
			t.Errorf("rule %q severity = %q, want %q", rule.Name, rule.Severity, "CRITICAL")
		}
		// Only the generic heuristic has a placeholder list; a provider's key
		// format is reported whatever the value says.
		if rule.Name == "hardcoded-credential" {
			if rule.Ignore == nil {
				t.Error("the hardcoded-credential rule has no placeholder list")
			}
			continue
		}
		if rule.Ignore != nil {
			t.Errorf("rule %q has a placeholder list, want none", rule.Name)
		}
	}
}
