package citest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_FixAgentCrashesSurviveGreenChecks(t *testing.T) {
	t.Parallel()
	for _, crashes := range []int{0, 2} {
		t.Run(fmt.Sprintf("crashes_%d", crashes), func(t *testing.T) {
			dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
			const red = `[{"name":"build","state":"FAILURE","bucket":"fail"}]`
			const green = `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
			var observations []string
			for i := 0; i < crashes; i++ {
				// One settled observation and one pre-repair snapshot per crash.
				observations = append(observations, red, red)
			}
			observations = append(observations, green)
			env, _ := stepstest.FakeCIGHLoggedSequence(t, "OPEN", observations, "MERGEABLE", "")
			calls := 0
			ag := &stepstest.MockAgent{AgentName: "test", RunFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				return nil, fmt.Errorf("crash %d: exit status 1 https://user:private-token@example.com API_KEY=abcdefghijklmnop\n\x1b[31m%s", calls, strings.Repeat("x", 4096))
			}}
			sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			prURL := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &prURL
			sctx.Repo.UpstreamURL = upstream
			sctx.Env = env
			sctx.Config.CITimeout = time.Minute
			sctx.Config.CI.ReleaseAfterChecksPass = true
			sctx.Config.AutoFix = config.AutoFix{CI: 2}
			step := (&steps.CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error {
				t.Fatal("scripted observations must settle without waiting")
				return nil
			})
			outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
			if err != nil || outcome == nil || outcome.NeedsApproval || outcome.AutoFixable || outcome.CIReadyNoCI == nil {
				t.Fatalf("green CI must complete: outcome=%+v err=%v", outcome, err)
			}
			if calls != crashes {
				t.Fatalf("fixer calls = %d, want %d", calls, crashes)
			}
			if crashes == 0 {
				if outcome.Findings != "" {
					t.Fatalf("no-crash control has findings: %s", outcome.Findings)
				}
				return
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil || len(findings.Items) != 1 {
				t.Fatalf("want one inspectable crash warning, got %q err=%v", outcome.Findings, err)
			}
			warning := findings.Items[0]
			if warning.Severity != types.FindingSeverityWarning || warning.Action != types.ActionNoOp {
				t.Fatalf("crash evidence must be a nonblocking warning: %+v", warning)
			}
			if !strings.Contains(warning.Description, "2 CI fix-agent crashes") || !strings.Contains(warning.Description, "crash 2: exit status 1") {
				t.Fatalf("missing count or last cause: %s", warning.Description)
			}
			for _, secret := range []string{"private-token", "abcdefghijklmnop", "\x1b", "\n"} {
				if strings.Contains(warning.Description, secret) {
					t.Errorf("warning contains unsanitized %q", secret)
				}
			}
			if len(warning.Description) > 2048 {
				t.Fatalf("warning is unbounded: %d bytes", len(warning.Description))
			}
		})
	}
}
