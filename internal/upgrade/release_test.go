package upgrade

// What the release job may do is decided by two files that no Go code reads:
// examples/deploy-gcp/ci-setup.sh, which grants the release account its
// roles, and examples/deploy-gcp/release.yml.example (installed as
// .github/workflows/release.yml), which is the only thing that runs as that
// account. Neither has a type checker, and the last revision of the pair let
// the ungated build job act as the Cloud Build default account, which holds
// run.admin and Secret Manager access on this project (adversarial F4). These
// tests are the check the files did not have: the script's dry run, driven
// through a stub gcloud that records every call and answers "not there" to
// every describe, must name exactly the three bindings the workflow needs and
// nothing project-wide, and the workflow must never submit a build or splice
// an expression into a shell script.
//
// They live in this package because the channel it reads (builds/<sha>/,
// canary/, latest/ and latest.json) is what that workflow publishes and what
// that account may write, which is the same reason upgrade_test.go pins the
// Makefile and install.sh from here; both files are reached the way those
// are.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// dryRun runs ci-setup.sh --dry-run with gcloud stubbed and returns the
// mutations it printed and the calls the stub saw. PATH holds the stub and
// the system directories only, so the real gcloud cannot be reached by
// accident: the contract forbids running this script for real, and a test
// that could is not one to have.
func dryRun(t *testing.T) (mutations []string, calls []string) {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "calls.log")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GCLOUD_LOG\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gcloud"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "ci-setup.sh", "--dry-run")
	cmd.Dir = filepath.Join("..", "..", "examples", "deploy-gcp")
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "GCLOUD_LOG=" + log, "HOME=" + bin, "REGION=asia-south1"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ci-setup.sh --dry-run: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "    gcloud ") {
			mutations = append(mutations, strings.TrimSpace(line))
		}
	}
	seen, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the stub was never called: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(seen)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return mutations, calls
}

// Two accounts, each holding the least it can. The release account, which
// any run of the repository can reach, may publish to one bucket EXCEPT the
// fleet's pointer, push to one repository, and be impersonated by that
// repository. The promote account may publish to the same bucket with no
// exception, and is reachable only from a job declaring the production
// environment. Neither holds a project-level role, anything on the Cloud
// Build account or its bucket, or actAs on any account, because a build
// config runs as the account that submits it and the default one can deploy
// Cloud Run revisions and read secrets.
func TestCISetupGrantsOnlyTheBindingsTheTwoJobsNeed(t *testing.T) {
	mutations, calls := dryRun(t)
	if len(mutations) == 0 {
		t.Fatal("the dry run printed no mutation; the stub answers every describe with 'not there', so each grant must be listed")
	}

	const (
		releaseSA = "loop-sessions-release@__PROJECT__.iam.gserviceaccount.com"
		promoteSA = "loop-sessions-promote@__PROJECT__.iam.gserviceaccount.com"
	)
	role := regexp.MustCompile(`--role=(\S+)`)
	byAccount := map[string][]string{}
	for _, m := range mutations {
		r := role.FindStringSubmatch(m)
		if r == nil {
			continue
		}
		account := ""
		switch {
		case strings.Contains(m, promoteSA):
			account = promoteSA
		case strings.Contains(m, releaseSA):
			account = releaseSA
		default:
			t.Errorf("a grant names neither account: %s", m)
			continue
		}
		byAccount[account] = append(byAccount[account], r[1])
	}
	for account, want := range map[string][]string{
		releaseSA: {"roles/artifactregistry.writer", "roles/iam.workloadIdentityUser", "roles/storage.objectAdmin"},
		promoteSA: {"roles/iam.workloadIdentityUser", "roles/storage.objectAdmin"},
	} {
		got := byAccount[account]
		sort.Strings(got)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("roles granted to %s = %v, want exactly %v\nmutations:\n  %s", account, got, want, strings.Join(mutations, "\n  "))
		}
	}

	for _, m := range mutations {
		for _, forbidden := range []string{
			"projects add-iam-policy-binding", // any project-wide role
			"cloudbuild",                      // the Cloud Build account or its source bucket
			"serviceAccountUser",              // actAs on anything
			"run.admin",                       // the deploy role r7 R2 proposed and the contract refused
		} {
			if strings.Contains(m, forbidden) {
				t.Fatalf("mutation %q grants what the release account must not hold (%s)", m, forbidden)
			}
		}
	}

	has := func(parts ...string) bool {
		for _, m := range mutations {
			ok := true
			for _, p := range parts {
				if !strings.Contains(m, p) {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
		return false
	}
	// The release account's binding carries the condition, and the removal
	// of its unconditional predecessor names the condition it is removing.
	// --all there would take the conditional binding with it and leave the
	// account unable to publish at all.
	if !has("storage buckets add-iam-policy-binding gs://__RELEASE_BUCKET__", "serviceAccount:"+releaseSA,
		"--role=roles/storage.objectAdmin", "title=no-latest",
		`expression=!resource.name.startsWith("projects/_/buckets/__RELEASE_BUCKET__/objects/latest/")`) {
		t.Errorf("the release account's objectAdmin binding is not conditioned away from latest/:\n  %s", strings.Join(mutations, "\n  "))
	}
	for _, m := range mutations {
		if strings.Contains(m, "remove-iam-policy-binding") && strings.Contains(m, "--all") {
			t.Errorf("a removal passes --all, which takes conditional bindings with it: %s", m)
		}
	}
	if !has("storage buckets add-iam-policy-binding gs://__RELEASE_BUCKET__", "serviceAccount:"+promoteSA, "--role=roles/storage.objectAdmin") {
		t.Errorf("no objectAdmin binding on the release bucket for the promote account:\n  %s", strings.Join(mutations, "\n  "))
	}
	for _, m := range mutations {
		if strings.Contains(m, promoteSA) && strings.Contains(m, "artifactregistry") {
			t.Errorf("the promote account is given a registry role it has no use for: %s", m)
		}
	}
	if !has("artifacts repositories add-iam-policy-binding loop-sessions", "--location=asia-south1", "--role=roles/artifactregistry.writer") {
		t.Errorf("no writer binding on the loop-sessions repository in asia-south1:\n  %s", strings.Join(mutations, "\n  "))
	}
	if !has("service-accounts add-iam-policy-binding "+releaseSA, "--role=roles/iam.workloadIdentityUser", "attribute.repository/__GITHUB_REPO__") {
		t.Errorf("workloadIdentityUser is not bound to the __GITHUB_REPO__ principal set:\n  %s", strings.Join(mutations, "\n  "))
	}
	// The promote account is reached through the composite attribute and
	// nothing else: a principal set names one attribute, so binding on the
	// environment alone would admit any repository's production.
	if !has("service-accounts add-iam-policy-binding "+promoteSA, "--role=roles/iam.workloadIdentityUser", "attribute.repo_env/__GITHUB_REPO__:production") {
		t.Errorf("the promote account is not bound to the repository's production environment:\n  %s", strings.Join(mutations, "\n  "))
	}
	for _, m := range mutations {
		if strings.Contains(m, promoteSA) && strings.Contains(m, "attribute.repository/") {
			t.Errorf("the promote account is reachable from any job of the repository: %s", m)
		}
	}
	if !has("providers create-oidc", "--attribute-condition=assertion.repository == '__GITHUB_REPO__'") {
		t.Errorf("the provider does not restrict itself to __GITHUB_REPO__:\n  %s", strings.Join(mutations, "\n  "))
	}
	// The mapping must read the environment through has(), because a mapping
	// that reads a claim the token lacks fails the exchange, and a push
	// job's token carries no environment at all.
	for _, m := range mutations {
		if !strings.Contains(m, "--attribute-mapping=") {
			continue
		}
		if !strings.Contains(m, "attribute.repo_env=") {
			t.Errorf("a provider mapping without repo_env: %s", m)
		}
		if !strings.Contains(m, "has(assertion.environment)") {
			t.Errorf("the mapping reads assertion.environment unguarded, which fails the exchange for a job with no environment: %s", m)
		}
	}

	// A dry run executes nothing: every call the stub saw is a describe or a
	// policy read. The stub exits 1 for all of them, which is what makes the
	// mutation list above complete.
	for _, c := range calls {
		if !strings.Contains(c, " describe ") && !strings.Contains(c, " get-iam-policy ") {
			t.Errorf("the dry run executed %q; only describes and policy reads may run", c)
		}
	}
}

// The script is a one-time setup a person may run twice; a second dry run
// against the same answers prints the same plan.
func TestCISetupDryRunIsRepeatable(t *testing.T) {
	first, _ := dryRun(t)
	second, _ := dryRun(t)
	if strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Fatalf("two dry runs differ:\n%s\n---\n%s", strings.Join(first, "\n"), strings.Join(second, "\n"))
	}
}

// release.yml is the only code that runs as the release account. Two things
// keep "cannot deploy" a property of the account rather than of the YAML:
// the server image is built and pushed from the runner, never submitted to
// Cloud Build (which would run it as the default build account), and no
// deploy verb appears at all. And a `${{ }}` expression is spliced into a
// `run:` script before the shell parses it, so a dispatch input reaches the
// shell only through `env:`, where it is a variable and nothing more.
func TestReleaseWorkflowNeverSubmitsABuildOrSplicesAnExpressionIntoShell(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", "deploy-gcp", "release.yml.example"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	indent := func(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }
	runIndent := -1 // the indent of the `run: |` line while inside its block
	for i, line := range lines {
		if runIndent >= 0 && (strings.TrimSpace(line) == "" || indent(line) > runIndent) {
			if strings.Contains(line, "${{") {
				t.Errorf("release.yml:%d: an expression inside a run block: %s", i+1, strings.TrimSpace(line))
			}
			continue
		}
		runIndent = -1
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		for _, forbidden := range []string{"builds submit", "gcloud run ", "--service-account"} {
			if strings.Contains(line, forbidden) {
				t.Errorf("release.yml:%d: %q must not appear: %s", i+1, forbidden, trimmed)
			}
		}
		switch {
		case strings.HasPrefix(trimmed, "run: |"):
			runIndent = indent(line)
		case strings.HasPrefix(trimmed, "run:"):
			if strings.Contains(line, "${{") {
				t.Errorf("release.yml:%d: an expression on a run line: %s", i+1, trimmed)
			}
		}
	}
	if !bytes.Contains(b, []byte("docker push")) {
		t.Error("release.yml does not push the server image from the runner")
	}
	if !bytes.Contains(b, []byte("PROMOTE: ${{ inputs.promote }}")) {
		t.Error("the promote input does not reach the shell through env")
	}
	// The two jobs authenticate as two accounts. The push job runs on every
	// merge and must not hold the identity that can move latest/.
	if !bytes.Contains(b, []byte("service_account: ${{ env.RELEASE_SA }}")) {
		t.Error("the push job does not authenticate as the release account")
	}
	if !bytes.Contains(b, []byte("service_account: ${{ env.PROMOTE_SA }}")) {
		t.Error("the promote job does not authenticate as the promote account")
	}
	if n := bytes.Count(b, []byte("service_account: ${{ env.RELEASE_SA }}")); n != 1 {
		t.Errorf("the release account authenticates %d jobs, want only the push job", n)
	}
	if !bytes.Contains(b, []byte(`grep -Eqx '[0-9a-f]{7}'`)) {
		t.Error("the promote input is not checked against the build directory shape before use")
	}
}
