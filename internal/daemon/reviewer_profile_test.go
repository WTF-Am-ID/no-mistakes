package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewerSelectionConcurrentLaunchAndNonceReplay(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "argv.log")
	bin := writeCapturingPiAgent(t, t.TempDir(), capture)
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&reviewRoleProbeStep{}} })
	writeConfig := func(model string) {
		t.Helper()
		if err := os.WriteFile(p.ConfigFile(), []byte("agent: pi\nagent_path_override:\n  pi: "+bin+"\nagent_config:\n  pi: {model: "+model+", effort: low}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("openai/default")
	requests := make([]*ipc.StartFreshRunParams, 2)
	for i, name := range []string{"a", "b"} {
		repo, head := setupTestGitRepo(t, p, d, "reviewer-"+name)
		requests[i] = &ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "main", HeadSHA: head, Intent: "independent reviewer", LaunchNonce: "reviewer-" + name, ValidationGeneration: "gen",
			PiProfile:       &agentcfg.PiProfile{Model: "openai/writer", Effort: agentcfg.EffortMedium},
			ReviewerProfile: &agentcfg.ReviewerProfile{Agent: types.AgentPi, Model: "openai/review-" + name, Effort: agentcfg.EffortHigh}}
	}
	var wg sync.WaitGroup
	results := make([]ipc.StartFreshRunResult, 2)
	for i := range requests {
		wg.Go(func() {
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Error(err)
				return
			}
			defer client.Close()
			if err := client.Call(ipc.MethodStartFreshRun, requests[i], &results[i]); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for i := range requests {
		run := waitForRunTerminalState(t, d, results[i].Receipt.RunID)
		if run.Status != types.RunCompleted || run.ReviewerProfile == nil || *run.ReviewerProfile != *requests[i].ReviewerProfile {
			t.Fatalf("launch lost selection: %+v", run)
		}
	}
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"openai/review-a", "openai/review-b", "openai/writer"} {
		want := 1
		if model == "openai/writer" {
			want = 2
		}
		if got := strings.Count(string(args), "--model "+model+" "); got != want {
			t.Fatalf("%s invocations=%d want=%d: %s", model, got, want, args)
		}
	}
	writeConfig("openai/later")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var replay ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartFreshRun, requests[0], &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Receipt.RunID != results[0].Receipt.RunID || replay.Receipt.ReviewerProfile == nil || *replay.Receipt.ReviewerProfile != *requests[0].ReviewerProfile {
		t.Fatalf("nonce replay drifted: %+v", replay)
	}
	changed := *requests[0].ReviewerProfile
	changed.Model = "openai/wrong"
	requests[0].ReviewerProfile = &changed
	if err := client.Call(ipc.MethodStartFreshRun, requests[0], &replay); err == nil {
		t.Fatal("nonce replay replaced reviewer")
	}
	var rerun ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: requests[1].RepoID, Branch: "main"}, &rerun); err != nil {
		t.Fatal(err)
	}
	inherited := waitForRunTerminalState(t, d, rerun.RunID)
	if inherited.Status != types.RunCompleted || inherited.ReviewerProfile == nil || *inherited.ReviewerProfile != *requests[1].ReviewerProfile || inherited.PiProfile != nil {
		t.Fatalf("rerun inheritance: %+v", inherited)
	}
}

func TestReviewerSelectionUnavailableDoesNotSupersedeActiveRun(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, head := setupTestGitRepo(t, p, d, "unavailable-reviewer")
	active, err := d.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: codex\nagent_path_override:\n  codex: "+filepath.Join(t.TempDir(), "missing-codex")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(d, p, nil)
	cancelled := false
	m.cancels[active.ID] = func(error) { cancelled = true }
	pin := &agentcfg.ReviewerProfile{Agent: types.AgentCodex, Model: "reviewer", Effort: agentcfg.EffortHigh}
	if _, err := m.startRun(context.Background(), repo, "feature", head, head, "test", nil, "review", "", false, "", nil, pin); err == nil || !strings.Contains(err.Error(), "reviewer unavailable") {
		t.Fatalf("unavailable reviewer accepted: %v", err)
	}
	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled || len(runs) != 1 || runs[0].ID != active.ID {
		t.Fatal("invalid reviewer superseded active run")
	}
}

func TestReviewerSelectionRecoveryUsesPersistedPin(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, head := setupTestGitRepo(t, p, d, "reviewer-recovery")
	pin := &agentcfg.ReviewerProfile{Agent: types.AgentPi, Model: "openai/reviewer", Effort: agentcfg.EffortHigh}
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", head, head, nil, "", "", "", "", false, nil, pin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: codex\nagent_config:\n  pi: {model: openai/later, effort: low}\nagent_args_override:\n  pi: [--model, later]\nreview_agents:\n  reviewer: {agent: codex}\n  reviewer_after_round: {agent: codex}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := p.WorktreeDir(repo.ID, run.ID)
	gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", dir, head)
	run, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := NewRunManager(d, p, nil).loadRecoveredConfig(context.Background(), run, repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	role := cfg.ForReviewAgent(cfg.ReviewAgents["reviewer"])
	if cfg.Agent != types.AgentCodex || role.Agent != pin.Agent || role.AgentProfile().Model != pin.Model || role.AgentProfile().Effort != pin.Effort || strings.Join(role.AgentArgsFor(pin.Agent), " ") != "--provider openai" || cfg.ReviewAgentTakeoverRound("reviewer_after_round") != 0 {
		t.Fatalf("recovery drifted: %+v", cfg)
	}
}

func TestReviewerSelectionRetryKeepsResolvedProfile(t *testing.T) {
	pin := &agentcfg.ReviewerProfile{Agent: types.AgentCodex, Model: "reviewer", Effort: agentcfg.EffortHigh}
	f := newRetryFixture(t, retryFixtureOptions{reviewer: pin})
	bin := writeMockClaude(t, t.TempDir())
	if err := os.WriteFile(f.p.ConfigFile(), []byte("agent: codex\nagent_path_override:\n  codex: "+bin+"\nagent_config:\n  codex: {model: later-default, effort: low}\nagent_args_override:\n  codex: [--model, different-reviewer]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(f.p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.RetryRunResult
	if err := client.Call(ipc.MethodRetryRun, &ipc.RetryRunParams{RepoID: f.repo.ID, Branch: "main", RunID: f.source.ID, CallerHeadSHA: f.callerHead}, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, f.d, result.RunID)
	if run.Status != types.RunCompleted || run.ReviewerProfile == nil || *run.ReviewerProfile != *pin {
		t.Fatalf("retry replaced reviewer: %+v", run)
	}
}
