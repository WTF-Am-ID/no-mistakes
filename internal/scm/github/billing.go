package github

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const billingRefusalAnnotation = "The job was not started because recent account payments have failed or your spending limit needs to be increased."

// markBillingFailures requires evidence for the exact job, not its name or
// duration. GitHub supplies no structured billing conclusion, so only its
// billing annotation together with an explicitly empty step list qualifies.
// Missing or unreadable evidence leaves the original failure untouched.
func (h *Host) markBillingFailures(ctx context.Context, checks []scm.Check, headSHA string) {
	for i, check := range checks {
		if !check.Failing() || check.Kind == scm.CheckKindStatus || check.App != "" && check.App != "github-actions" {
			continue
		}
		runID, jobID, ok := h.actionsRerunTarget(check.Link)
		if !ok || jobID == "" {
			continue
		}
		var job struct {
			ID          int64            `json:"id"`
			RunID       int64            `json:"run_id"`
			HeadSHA     string           `json:"head_sha"`
			Status      string           `json:"status"`
			Conclusion  string           `json:"conclusion"`
			Steps       *[]githubJobStep `json:"steps"`
			CheckRunURL string           `json:"check_run_url"`
		}
		if !h.readBillingEvidence(ctx, "repos/"+h.repoSlug()+"/actions/jobs/"+jobID, &job) ||
			strconv.FormatInt(job.ID, 10) != jobID || strconv.FormatInt(job.RunID, 10) != runID ||
			job.Status != "completed" || job.Conclusion != "failure" ||
			job.Steps == nil || len(*job.Steps) != 0 || headSHA != "" && job.HeadSHA != headSHA {
			continue
		}
		// The job API binds the Actions job to its check run. Never read an
		// arbitrary URL from provider output, or borrow a same-named job's data.
		prefix := "https://api.github.com/repos/" + h.repoSlug() + "/check-runs/"
		if h.host != "" && h.host != "github.com" {
			prefix = "https://" + h.host + "/api/v3/repos/" + h.repoSlug() + "/check-runs/"
		}
		checkID := strings.TrimPrefix(job.CheckRunURL, prefix)
		if !strings.HasPrefix(job.CheckRunURL, prefix) || !isNumericID(checkID) || check.ProviderID != "" && check.ProviderID != "github-check-run:"+checkID {
			continue
		}
		var pages [][]struct {
			Level   string `json:"annotation_level"`
			Message string `json:"message"`
		}
		if !h.readBillingEvidence(ctx, "repos/"+h.repoSlug()+"/check-runs/"+checkID+"/annotations", &pages, "--paginate", "--slurp") {
			continue
		}
		for _, annotations := range pages {
			for _, annotation := range annotations {
				if annotation.Level == "failure" && strings.HasPrefix(strings.Join(strings.Fields(annotation.Message), " "), billingRefusalAnnotation) {
					// Retain only the recognized provider diagnosis. Raw details,
					// paths and extra annotation prose are not publishable evidence.
					checks[i].InfrastructureFailure = billingRefusalAnnotation
				}
			}
		}
	}
}

func (h *Host) readBillingEvidence(ctx context.Context, endpoint string, result any, flags ...string) bool {
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, endpoint)
	args = append(args, flags...)
	out, err := h.cmd(ctx, "gh", args...).Output()
	return err == nil && json.Unmarshal(out, result) == nil
}
