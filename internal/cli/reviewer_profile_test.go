package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/spf13/cobra"
)

func TestReviewerSelectionLaunchFlags(t *testing.T) {
	for _, cmd := range []*cobra.Command{newAxiRunCmd(), newRerunCmd()} {
		if err := cmd.ParseFlags([]string{"--reviewer", "codex", "--reviewer-model", "review-model", "--reviewer-effort", "high"}); err != nil {
			t.Fatalf("%s does not accept an independent reviewer: %v", cmd.Name(), err)
		}
	}
}

func TestReviewerSelectionFlagsPushOptionsAndStatus(t *testing.T) {
	cmd := newAxiRunCmd()
	if err := cmd.ParseFlags([]string{"--reviewer", "codex", "--reviewer-model", "reviewer", "--reviewer-effort", "high"}); err != nil {
		t.Fatal(err)
	}
	pin, err := reviewerFromFlags(cmd)
	if err != nil {
		t.Fatal(err)
	}
	options := formatReviewerPushOptions(pin)
	decoded, err := parseReviewerPushOptions(options)
	if err != nil || !reflect.DeepEqual(decoded, pin) {
		t.Fatalf("round trip: %+v %v", decoded, err)
	}
	if _, err := parseReviewerPushOptions(append(options, options...)); err == nil {
		t.Fatal("duplicate push option accepted")
	}
	database, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "h", "b", nil, "", "", "", "", false, nil, pin)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range []runView{runViewFromDB(run, nil, database), runViewFromIPC(&ipc.RunInfo{ID: run.ID, ReviewerProfile: pin})} {
		var out bytes.Buffer
		render := &cobra.Command{}
		render.SetOut(&out)
		emitDoc(render, runObjectField(view))
		for _, want := range []string{"reviewer_profile:", "agent: codex", "model: reviewer", "effort: high"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("status missing %q: %s", want, out.String())
			}
		}
	}
	var out bytes.Buffer
	if err := renderRunAgentPerf(&out, database, run.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "pinned reviewer: agent=codex model=reviewer effort=high") {
		t.Fatalf("stats lost reviewer: %s", out.String())
	}
	out.Reset()
	render := &cobra.Command{}
	render.SetOut(&out)
	emitLaunchReceipt(render, ipc.LaunchReceipt{PiProfile: &agentcfg.PiProfile{Model: "openai/writer", Effort: agentcfg.EffortLow}, ReviewerProfile: pin})
	for _, want := range []string{"pi_profile:", "model: openai/writer", "reviewer_profile:", "model: reviewer"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("receipt missing %q: %s", want, out.String())
		}
	}
}

func TestReviewerSelectionBadFlagsRefuseWithoutLeakingInput(t *testing.T) {
	for _, args := range [][]string{
		{"--reviewer="}, {"--reviewer-model", "reviewer"},
		{"--reviewer", "auto"}, {"--reviewer", "codex", "--reviewer-effort="},
		{"--reviewer", "codex", "--reviewer-model", "https://secret@host/model"},
		{"--reviewer", "codex", "--reviewer-effort", "secret"},
	} {
		cmd := newAxiRunCmd()
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		if _, err := reviewerFromFlags(cmd); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid flags error: %v", err)
		}
	}
	cmd := newAxiRunCmd()
	if err := cmd.ParseFlags([]string{"--reviewer", "codex", "--reviewer", "pi"}); err == nil {
		t.Fatal("repeated reviewer accepted")
	}
	if pin, err := reviewerFromFlags(newAxiRunCmd()); err != nil || pin != nil {
		t.Fatalf("omission invented reviewer: %+v %v", pin, err)
	}
	pin := &agentcfg.ReviewerProfile{Agent: "codex", Model: "reviewer", Effort: agentcfg.EffortHigh}
	if !pin.Matches(nil) || !pin.Matches(&agentcfg.ReviewerProfile{Agent: "codex"}) || pin.Matches(&agentcfg.ReviewerProfile{Agent: "pi"}) {
		t.Fatal("reattachment changed reviewer identity")
	}
}
