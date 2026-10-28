package github

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestBillingFailuresRequireExactJobAndAnnotationEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		edit            func(map[string]any)
		annotations     string
		jobError        bool
		annotationError bool
		want            bool
	}{
		{name: "never started billing refusal", want: true},
		{name: "annotation extra prose is discarded", annotations: `[[{"annotation_level":"failure","message":"` + billingRefusalAnnotation + ` private-marker","raw_details":"private-marker","path":"/Users/private-marker"}]]`, want: true},
		{name: "one second test failure", edit: func(j map[string]any) {
			j["started_at"] = "2026-10-28T00:00:00Z"
			j["completed_at"] = "2026-10-28T00:00:01Z"
			j["steps"] = []any{map[string]any{"number": 1, "conclusion": "success"}, map[string]any{"number": 2, "conclusion": "failure"}}
		}},
		{name: "missing steps", edit: func(j map[string]any) { delete(j, "steps") }},
		{name: "null steps", edit: func(j map[string]any) { j["steps"] = nil }},
		{name: "queued job", edit: func(j map[string]any) { j["status"] = "queued" }},
		{name: "successful job", edit: func(j map[string]any) { j["conclusion"] = "success" }},
		{name: "wrong job", edit: func(j map[string]any) { j["id"] = 3 }},
		{name: "wrong run", edit: func(j map[string]any) { j["run_id"] = 9 }},
		{name: "wrong head", edit: func(j map[string]any) { j["head_sha"] = "old-head" }},
		{name: "wrong check", edit: func(j map[string]any) { j["check_run_url"] = "https://api.github.com/repos/test/repo/check-runs/23" }},
		{name: "wrong repository", edit: func(j map[string]any) { j["check_run_url"] = "https://api.github.com/repos/other/repo/check-runs/22" }},
		{name: "missing check link", edit: func(j map[string]any) { delete(j, "check_run_url") }},
		{name: "bare check id is not a URL", edit: func(j map[string]any) { j["check_run_url"] = "22" }},
		{name: "unreadable job", jobError: true},
		{name: "unreadable annotations", annotationError: true},
		{name: "no annotations", annotations: `[[]]`},
		{name: "null annotations", annotations: `null`},
		{name: "malformed annotations", annotations: `{`},
		{name: "missing annotation fields", annotations: `[[{}]]`},
		{name: "unrelated annotation", annotations: `[[{"annotation_level":"failure","message":"Test failed in one second"}]]`},
		{name: "billing mention is not a refusal", annotations: `[[{"annotation_level":"failure","message":"Test expected: ` + billingRefusalAnnotation + `"}]]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			job := map[string]any{
				"id": 2, "run_id": 1, "head_sha": "head", "status": "completed", "conclusion": "failure",
				"steps": []any{}, "check_run_url": "https://api.github.com/repos/test/repo/check-runs/22",
			}
			if tc.edit != nil {
				tc.edit(job)
			}
			payload, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}
			annotations := tc.annotations
			if annotations == "" {
				annotations = `[[{"annotation_level":"failure","message":"` + billingRefusalAnnotation + `"}]]`
			}
			responses := map[string]githubTestResponse{
				"gh api repos/test/repo/actions/jobs/2":                               {stdout: string(payload)},
				"gh api repos/test/repo/check-runs/22/annotations --paginate --slurp": {stdout: annotations},
			}
			if tc.jobError {
				responses["gh api repos/test/repo/actions/jobs/2"] = githubTestResponse{stderr: "HTTP 403", code: 1}
			}
			if tc.annotationError {
				responses["gh api repos/test/repo/check-runs/22/annotations --paginate --slurp"] = githubTestResponse{stderr: "HTTP 403", code: 1}
			}
			host := New(githubTestCmdFactory(responses), nil, "", "test/repo")
			checks := []scm.Check{{Name: "build", ProviderID: "github-check-run:22", Bucket: scm.CheckBucketFail, Kind: scm.CheckKindRun, State: "FAILURE", App: "github-actions", Link: "https://github.com/test/repo/actions/runs/1/job/2"}}
			host.markBillingFailures(context.Background(), checks, "head")
			if got := checks[0].InfrastructureFailure; (got != "") != tc.want || tc.want && got != billingRefusalAnnotation {
				t.Fatalf("diagnosis=%q, want billing=%v", got, tc.want)
			}
			if !checks[0].Failing() || checks[0].State != "FAILURE" || checks[0].PreRunFailure {
				t.Fatalf("classification changed the failure verdict: %+v", checks[0])
			}
		})
	}
}

func TestBillingFailuresNeverBorrowSameNamedJobEvidence(t *testing.T) {
	t.Parallel()
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh api repos/test/repo/actions/jobs/2":                               {stdout: `{"id":2,"run_id":1,"head_sha":"head","status":"completed","conclusion":"failure","steps":[],"check_run_url":"https://api.github.com/repos/test/repo/check-runs/22"}`},
		"gh api repos/test/repo/check-runs/22/annotations --paginate --slurp": {stdout: `[[{"annotation_level":"failure","message":"` + billingRefusalAnnotation + `"}]]`},
		// An empty run job list cannot establish that job 3 never executed.
		"gh run view 1 --repo test/repo --json jobs": {stdout: `{"jobs":[]}`},
	}), nil, "", "test/repo")
	checks := []scm.Check{
		{Name: "build", ProviderID: "github-check-run:22", Bucket: scm.CheckBucketFail, Link: "https://github.com/test/repo/actions/runs/1/job/2"},
		{Name: "build", ProviderID: "github-check-run:23", Bucket: scm.CheckBucketFail, Link: "https://github.com/test/repo/actions/runs/1/job/3"},
		{Name: "build", ProviderID: "github-workflow-run:1", Bucket: scm.CheckBucketFail, Link: "https://github.com/test/repo/actions/runs/1"},
	}
	host.markBillingFailures(context.Background(), checks, "head")
	if checks[0].InfrastructureFailure != billingRefusalAnnotation || checks[1].InfrastructureFailure != "" || checks[2].InfrastructureFailure != "" {
		t.Fatalf("classification borrowed evidence between jobs: %+v", checks)
	}
}

func TestGetChecksRetainsSanitizedBillingEvidenceAndFailure(t *testing.T) {
	t.Parallel()
	host := New(githubTestCmdFactory(map[string]githubTestResponse{
		"gh pr checks 123 --repo test/repo --json name,state,bucket,completedAt,link": {stdout: `[{"name":"build","state":"FAILURE","bucket":"fail","link":"https://github.com/test/repo/actions/runs/1/job/2"}]`},
		"gh api repos/test/repo/actions/jobs/2":                                       {stdout: `{"id":2,"run_id":1,"status":"completed","conclusion":"failure","steps":[],"check_run_url":"https://api.github.com/repos/test/repo/check-runs/22"}`},
		"gh api repos/test/repo/check-runs/22/annotations --paginate --slurp":         {stdout: `[[{"annotation_level":"failure","message":"` + billingRefusalAnnotation + ` private-marker","raw_details":"private-marker"}]]`},
	}), nil, "", "test/repo")
	checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "123"})
	if err != nil || len(checks) != 1 || !checks[0].Failing() || checks[0].InfrastructureFailure != billingRefusalAnnotation {
		t.Fatalf("checks=%+v err=%v", checks, err)
	}
	encoded, err := json.Marshal(checks)
	if err != nil || strings.Contains(string(encoded), "private-marker") {
		t.Fatalf("unsanitized evidence: %s err=%v", encoded, err)
	}
}
