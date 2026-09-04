package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"slop-cannon/internal/agents"
	"slop-cannon/internal/domain"
	"slop-cannon/internal/events"
	"slop-cannon/internal/gitx"
	"slop-cannon/internal/logging"
)

// P9-05's Do clause — "prompts never include .env*, *.pem, id_* or files
// matched by a configurable exclude list" — is about the prompts a *run*
// builds, not the ones a test builds. The filter itself is tested in
// internal/agents; what these tests pin is the wiring: the loop that turns a
// change set into a tester prompt, the bd comment it writes beside it, and
// the pull request body the delivery hands to `gh`.
//
// Every one of them used to be unfiltered. The filter had no production
// caller at all outside the repository scan.

const (
	// p9Password is what an engineer wrote into .env, and p9Key what it
	// wrote into a PEM file. Both are long enough for the credential rules
	// and distinctive enough to grep a whole prompt for.
	p9Password = "hunter2-hunter2-hunter2"
	p9Key      = "plantedprivatekeymaterial"
	// p9Token is a credential an agent echoed in its own prose rather than
	// in a file: the value half of P9-05.
	p9Token = "sk-ant-api03-CANARYCANARYCANARY"
)

// p9Filter is the run's filter with one configured `exclude` pattern beside
// the built-in ones, so both halves of the list are exercised.
func p9Filter(t *testing.T) *agents.SecretFilter {
	t.Helper()
	f, err := agents.NewSecretFilter("secrets/*")
	require.NoError(t, err)
	return f
}

// p9SecretDiff is the change set of a round in which the engineer touched
// the reviewed file, a dotenv file, a PEM key and a configured exclude path.
// Only the first may reach a prompt.
func p9SecretDiff(head string) *gitx.Diff {
	stats := []gitx.FileStat{
		{Path: "retry.go", Insertions: 20},
		{Path: ".env", Insertions: 1},
		{Path: "certs/server.pem", Insertions: 1},
		{Path: "secrets/deploy.txt", Insertions: 1},
	}
	files := make([]gitx.DiffFile, 0, len(stats))
	for _, s := range stats {
		files = append(files, gitx.DiffFile{FileStat: s, Status: gitx.DiffAdded})
	}
	patch := strings.Join([]string{
		"diff --git a/retry.go b/retry.go",
		"--- a/retry.go",
		"+++ b/retry.go",
		"@@ -1,2 +1,3 @@",
		"+for {",
		"+  break",
		"+}",
		"diff --git a/.env b/.env",
		"--- /dev/null",
		"+++ b/.env",
		"@@ -0,0 +1 @@",
		"+DATABASE_PASSWORD=" + p9Password,
		"diff --git a/certs/server.pem b/certs/server.pem",
		"--- /dev/null",
		"+++ b/certs/server.pem",
		"@@ -0,0 +1,3 @@",
		"+-----BEGIN RSA PRIVATE KEY-----",
		"+" + p9Key,
		"+-----END RSA PRIVATE KEY-----",
		"diff --git a/secrets/deploy.txt b/secrets/deploy.txt",
		"--- /dev/null",
		"+++ b/secrets/deploy.txt",
		"@@ -0,0 +1 @@",
		"+" + p9Token,
		"",
	}, "\n")
	return &gitx.Diff{
		Stats: gitx.DiffStats{Base: "base0", Head: head, Files: len(stats), Insertions: 23, Changed: stats},
		Files: files,
		Patch: patch,
	}
}

// TestFeedbackNeverPutsAWithheldPathInAPrompt is P9-05's Done-when driven
// through the loop that actually builds the prompts. The change set carries
// a dotenv file, a PEM key and a configured `exclude` path beside the file
// under review; the tester that reviews it, the engineer that answers it and
// the round after them must see none of the four.
func TestFeedbackNeverPutsAWithheldPathInAPrompt(t *testing.T) {
	item := fbItemAt("qa-r1-1", "retry loop never terminates", "retry.go", domain.SeverityMajor, domain.ItemOpen)
	sc := fbScenario("planted-secrets")
	fbSay(sc, "eng-sc-fb1-r1", fbReport("added the break"))
	fbSay(sc, "tester-ux-sc-fb1-r1", fbApprove("reads fine"))
	fbSay(sc, "tester-test-sc-fb1-r1", fbApprove("covered"))
	fbSay(sc, "tester-qa-sc-fb1-r1", fbChanges("it spins", item))
	fbSay(sc, "eng-sc-fb1-r2", fbResponses(domain.Response{
		ItemID: "qa-r1-1", Action: domain.ActionFixed, Rationale: "added the break",
	}))
	resolved := item
	resolved.Status = domain.ItemResolved
	fbSay(sc, "tester-qa-sc-fb1-r2", fbSettled("the break is there", resolved))

	// The D-09 ratio is measured on the diff gitx returned, not on the one
	// the prompt shows: SecretFilter.GitDiff's own doc comment says so, and
	// a filtered total would quietly inflate every ratio.
	var (
		mu        sync.Mutex
		ratioSeen []int
	)
	rig := fbSetup(t, sc, func(o *FeedbackOptions) {
		o.Filter = p9Filter(t)
		inner := o.Ratio
		o.Ratio = func(ctx context.Context, id domain.BeadID, prev, cur *gitx.Diff) (gitx.ChangeRatio, error) {
			mu.Lock()
			ratioSeen = append(ratioSeen, cur.ChangedLines())
			mu.Unlock()
			return inner(ctx, id, prev, cur)
		}
	})
	rig.mu.Lock()
	rig.diff = p9SecretDiff("head1")
	rig.mu.Unlock()
	rig.pump(t, 12)

	require.Equal(t, domain.BeadStateApproved, rig.Thread().State)

	withheld := []string{p9Password, p9Key, p9Token, ".env", "server.pem", "deploy.txt"}
	fired := rig.Fired()
	require.NotEmpty(t, fired)
	for _, agent := range fired {
		for _, p := range rig.Prompts(agent) {
			for _, secret := range withheld {
				assert.NotContains(t, p, secret, "%s's prompt leaks %q", agent, secret)
			}
		}
	}

	// Non-vacuity: the reviewed file is still there, and the tester is told
	// the change set is partial rather than shown a diff that pretends the
	// withheld files do not exist.
	first := rig.Prompt("tester-qa-sc-fb1-r1")
	assert.Contains(t, first, "retry.go")
	assert.Contains(t, first, "+  break")
	assert.Contains(t, first, agents.SecretPlaceholder)

	// The round-two prompts quote round one's history, whose diffstat names
	// every changed path. That is a second, separate leak: SecretFilter.Diff
	// understands a patch, not a stat, so the stat is filtered where the
	// round is filed.
	second := rig.Prompt("eng-sc-fb1-r2")
	assert.Contains(t, second, "retry.go", "the history is still quoted")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, ratioSeen, "the D-09 measurement ran")
	for _, total := range ratioSeen {
		assert.Equal(t, 23, total, "D-09 is measured on the unfiltered change set")
	}
}

// TestFeedbackMasksACredentialAnAgentEchoed is the value half: a token in an
// agent's own prose is masked as the reply arrives, so the bead comment, the
// thread snapshot the pull request body renders, and every later prompt all
// carry the masked text rather than the credential.
func TestFeedbackMasksACredentialAnAgentEchoed(t *testing.T) {
	sc := fbScenario("echoed-credential")
	fbSay(sc, "eng-sc-fb1-r1", domain.ImplReport{
		Summary:      "exported ANTHROPIC_API_KEY=" + p9Token + " to reproduce",
		FilesChanged: []string{"retry.go"},
		HowToVerify:  "go test ./...",
	})
	for _, l := range domain.Lenses() {
		fbSay(sc, "tester-"+l.String()+"-sc-fb1-r1",
			fbApprove("fine, though ANTHROPIC_API_KEY="+p9Token+" is in the report"))
	}
	rig := fbSetup(t, sc, func(o *FeedbackOptions) { o.Filter = p9Filter(t) })
	rig.pump(t, 8)

	th := rig.Thread()
	require.Equal(t, domain.BeadStateApproved, th.State)
	require.NotNil(t, th.Report)
	assert.NotContains(t, th.Report.Summary, p9Token, "the thread the pull request body renders")
	assert.Contains(t, th.Report.Summary, logging.Redacted)
	require.NotEmpty(t, th.Verdicts)
	for _, v := range th.Verdicts {
		assert.NotContains(t, v.Summary, p9Token)
	}

	// D-06: the same masked text is what bd records.
	msgs := rig.Messages.All()
	require.NotEmpty(t, msgs)
	for _, m := range msgs {
		assert.NotContains(t, p9MessageText(t, m.Payload), p9Token,
			"the %s comment on the bead carries the credential", m.Type)
	}

	// And what the next prompt quotes.
	for _, agent := range rig.Fired() {
		for _, p := range rig.Prompts(agent) {
			assert.NotContains(t, p, p9Token, "%s's prompt leaks the credential", agent)
		}
	}

	// The events were already covered by P9-09; assert it here too, because
	// the redaction now happens before the event is built rather than only
	// at the publication point.
	for _, ev := range rig.Events(events.KindVerdictReceived) {
		v, ok := ev.(events.VerdictReceived)
		require.True(t, ok)
		assert.NotContains(t, v.Verdict.Summary, p9Token)
	}
}

// p9MessageText renders a stored payload as text for a substring assertion.
func p9MessageText(t *testing.T, payload any) string {
	t.Helper()
	var b strings.Builder
	switch v := payload.(type) {
	case domain.ImplReport:
		b.WriteString(v.Summary + "\n" + v.HowToVerify + "\n")
		b.WriteString(strings.Join(v.Risks, "\n") + strings.Join(v.Questions, "\n"))
	case domain.Verdict:
		b.WriteString(v.Summary + "\n")
		for _, it := range v.Items {
			b.WriteString(it.Title + "\n" + it.Detail + "\n" + it.Suggestion + "\n")
		}
	case []domain.Response:
		for _, r := range v {
			b.WriteString(r.Rationale + "\n")
		}
	default:
		t.Fatalf("unexpected payload %T", payload)
	}
	return b.String()
}

// TestDeliveryMasksACredentialInThePullRequestBody covers the one sink that
// leaves the machine. The report and the verdicts quoted in a pull request
// body come from the feedback thread, which a resumed run rebuilds from bd's
// comments, so the delivery masks them again rather than trusting whoever
// filled the request.
func TestDeliveryMasksACredentialInThePullRequestBody(t *testing.T) {
	t.Parallel()
	h := newDeliveryHarness(t, true, func(o *DeliveryOptions, _ *deliveryHarness) {
		o.Filter = p9Filter(t)
	})
	bead := deliveryBead(domain.BeadStateApproved)
	bead.Description = "the deploy key is " + p9Key
	report := domain.ImplReport{
		Summary:      "set ANTHROPIC_API_KEY=" + p9Token + " in the environment",
		FilesChanged: []string{"retry.go"},
		HowToVerify:  "AWS_SECRET_ACCESS_KEY=" + p9Password + " make test",
	}
	out, err := h.Deliver(context.Background(), DeliveryRequest{
		Bead:   bead,
		Report: &report,
		Verdicts: []domain.Verdict{{
			Lens: domain.LensQA, Outcome: domain.VerdictApprove,
			// The assignment rule is anchored to the start of a line, which
			// is what keeps it from firing inside an expression, so the
			// verdict quotes the offending line the way a tester would.
			Summary: "the committed line reads\nDATABASE_PASSWORD=" + p9Password,
		}},
	})
	require.NoError(t, err)
	require.Equal(t, DeliveryPending, out.Kind, out.Summary())

	h.pr.mu.Lock()
	calls := append([]gitx.PRRequest(nil), h.pr.calls...)
	h.pr.mu.Unlock()
	require.Len(t, calls, 1)
	body := gitx.PRBody(calls[0], "sc/bead", "main")
	for _, secret := range []string{p9Token, p9Password} {
		assert.NotContains(t, body, secret, "the pull request body leaks %q to the remote", secret)
	}
	assert.Contains(t, body, logging.Redacted)
	assert.Contains(t, body, "retry.go", "the body still says what changed")

	// The caller's own request is untouched: the filter copies.
	assert.Contains(t, report.Summary, p9Token)
}
