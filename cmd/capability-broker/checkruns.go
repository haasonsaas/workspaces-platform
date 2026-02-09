package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	workspacesv1alpha1 "workspaces-platform/api/v1alpha1"
)

func (s *server) startCheckRunReporter() {
	if s.gh == nil || !s.gh.enableCheckRuns {
		return
	}
	go s.checkRunReporterLoop()
}

func (s *server) checkRunReporterLoop() {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		s.reportCompletedCheckRuns(ctx)
		cancel()
		<-t.C
	}
}

func (s *server) reportCompletedCheckRuns(ctx context.Context) {
	var list workspacesv1alpha1.AgentJobList
	if err := s.k8s.List(ctx, &list, ctrlclient.InNamespace("agents")); err != nil {
		log.Printf("checkrun reporter list agentjobs: %v", err)
		return
	}

	for i := range list.Items {
		aj := &list.Items[i]
		if aj.Annotations == nil {
			continue
		}

		idStr := strings.TrimSpace(aj.Annotations["workspaces.platform.dev/github-check-run-id"])
		if idStr == "" {
			continue
		}
		if strings.TrimSpace(aj.Annotations["workspaces.platform.dev/github-check-run-completed-at"]) != "" {
			continue
		}

		phase := strings.TrimSpace(aj.Status.Phase)
		if phase != "Succeeded" && phase != "Failed" {
			continue
		}

		repo := strings.ToLower(strings.TrimSpace(aj.Annotations["workspaces.platform.dev/github-repo"]))
		if repo == "" || !strings.Contains(repo, "/") {
			continue
		}
		if !s.gh.repoIsAllowed(repo) {
			continue
		}

		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			continue
		}

		conclusion := "success"
		if phase == "Failed" {
			conclusion = "failure"
		}

		jobName := strings.TrimSpace(aj.Status.JobName)
		if jobName == "" {
			jobName = fmt.Sprintf("agentjob-%s", aj.Name)
		}
		summary := fmt.Sprintf("AgentJob `%s/%s` %s.\n\nFetch logs:\n`kubectl -n %s logs job/%s`\n",
			strings.TrimSpace(aj.Namespace),
			strings.TrimSpace(aj.Name),
			strings.ToLower(phase),
			strings.TrimSpace(aj.Namespace),
			jobName,
		)

		if _, err := s.gh.completeCheckRun(ctx, repo, id, conclusion, summary); err != nil {
			log.Printf("checkrun reporter update failed repo=%s id=%d: %v", repo, id, err)
			continue
		}

		if s.audit != nil {
			s.audit.Emit("github.checkrun.complete", map[string]any{
				"repo":        repo,
				"check_run_id": id,
				"namespace":   aj.Namespace,
				"agentjob":    aj.Name,
				"conclusion":  conclusion,
			})
		}

		patchBase := aj.DeepCopy()
		if aj.Annotations == nil {
			aj.Annotations = map[string]string{}
		}
		aj.Annotations["workspaces.platform.dev/github-check-run-completed-at"] = time.Now().UTC().Format(time.RFC3339Nano)
		if err := s.k8s.Patch(ctx, aj, ctrlclient.MergeFrom(patchBase)); err != nil {
			log.Printf("checkrun reporter patch agentjob %s/%s: %v", aj.Namespace, aj.Name, err)
		}
	}
}
