package daemon

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// Validate availability and trusted instruction suppression before cancellation
// or row creation. An unavailable explicit reviewer must never become a fallback.
func (m *RunManager) validateReviewerBeforeCancel(ctx context.Context, repo *db.Repo, head string, global *config.GlobalConfig, pin *agentcfg.ReviewerProfile) error {
	cfg, _, err := m.effectiveConfigBeforeRun(ctx, repo, head, global, "reviewer selection")
	if err != nil {
		return err
	}
	if err := cfg.ApplyReviewerProfile(pin); err != nil {
		return err
	}
	role := cfg.ForReviewAgent(cfg.ReviewAgents[config.RoleReviewer])
	role.QuotaRouting.Enabled = false
	ag, _, err := newConfiguredAgent(ctx, role, m.paths.EvidenceRoot(cfg.Test.Evidence.LocalRoot), exec.LookPath, runenv.Overlay{}, nil)
	if err != nil {
		return fmt.Errorf("requested reviewer unavailable: %w", err)
	}
	return ag.Close()
}
