package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		ns := strings.TrimSpace(aj.Namespace)

		// Best-effort: enrich the check-run with concrete runtime info and a log capture
		// (redacted and size-capped), optionally uploading to an artifact store.
		var (
			podName    string
			exitCode   *int32
			exitReason string
			duration   time.Duration

			logBytes       []byte
			logTruncated   bool
			artifactLogKey string
			artifactLogURL string
			artifactResKey string
			artifactResURL string
		)

		job, pod, jerr := s.fetchJobAndPod(ctx, ns, jobName)
		if jerr == nil && pod != nil {
			podName = pod.Name
			exitCode, exitReason = extractContainerExit(pod, "agent")

			// 64KiB cap; agent-runner already caps, but we fail-closed here too.
			logBytes, logTruncated, _ = s.fetchPodLogs(ctx, ns, podName, "agent", 64<<10)
		}
		if jerr == nil && job != nil && job.Status.StartTime != nil && job.Status.CompletionTime != nil {
			duration = job.Status.CompletionTime.Sub(job.Status.StartTime.Time).Truncate(time.Second)
		}

		if s.artifacts != nil && s.artifacts.Enabled() {
			uid := strings.TrimSpace(string(aj.UID))
			if uid == "" {
				uid = "unknown"
			}

			if len(logBytes) != 0 {
				artifactLogKey = s.artifacts.Key("agentjobs", ns, aj.Name, uid, "agent.log")
				if err := s.artifacts.Put(ctx, artifactLogKey, "text/plain; charset=utf-8", logBytes); err == nil {
					if u, ok, _ := s.artifacts.PresignGet(ctx, artifactLogKey); ok {
						artifactLogURL = u
					}
				}
			}

			result := map[string]any{
				"namespace":    ns,
				"name":         strings.TrimSpace(aj.Name),
				"phase":        phase,
				"jobName":      jobName,
				"podName":      podName,
				"logTruncated": logTruncated,
				"capturedAt":   time.Now().UTC().Format(time.RFC3339Nano),
			}
			if exitCode != nil {
				result["exitCode"] = *exitCode
			}
			if strings.TrimSpace(exitReason) != "" {
				result["exitReason"] = strings.TrimSpace(exitReason)
			}
			if strings.TrimSpace(repo) != "" {
				result["githubRepo"] = repo
			}
			if raw := strings.TrimSpace(aj.Annotations["workspaces.platform.dev/github-pr-number"]); raw != "" {
				if n, err := strconv.Atoi(raw); err == nil && n > 0 {
					result["githubPullNumber"] = n
				}
			}
			if sha := strings.TrimSpace(aj.Annotations["workspaces.platform.dev/github-head-sha"]); sha != "" {
				result["githubHeadSHA"] = sha
			}

			if b, err := json.Marshal(result); err == nil {
				artifactResKey = s.artifacts.Key("agentjobs", ns, aj.Name, uid, "result.json")
				if err := s.artifacts.Put(ctx, artifactResKey, "application/json", b); err == nil {
					if u, ok, _ := s.artifacts.PresignGet(ctx, artifactResKey); ok {
						artifactResURL = u
					}
				}
			}
		}

		phaseLower := strings.ToLower(phase)
		summary := fmt.Sprintf("AgentJob `%s/%s` %s.\n\nFetch logs:\n`kubectl -n %s logs job/%s`\n",
			ns,
			strings.TrimSpace(aj.Name),
			phaseLower,
			ns,
			jobName,
		)
		if artifactLogURL != "" {
			summary += "\nArtifacts:\n- agent.log: " + artifactLogURL + "\n"
		} else if artifactLogKey != "" {
			summary += "\nArtifacts:\n- agent.log key: `" + artifactLogKey + "`\n"
		}

		textLines := []string{
			fmt.Sprintf("- AgentJob: `%s/%s`", ns, strings.TrimSpace(aj.Name)),
			fmt.Sprintf("- Job: `%s`", jobName),
		}
		if podName != "" {
			textLines = append(textLines, fmt.Sprintf("- Pod: `%s`", podName))
		}
		if duration > 0 {
			textLines = append(textLines, fmt.Sprintf("- Duration: `%s`", duration.String()))
		}
		if exitCode != nil {
			line := fmt.Sprintf("- Exit code: `%d`", *exitCode)
			if strings.TrimSpace(exitReason) != "" {
				line += fmt.Sprintf(" (%s)", strings.TrimSpace(exitReason))
			}
			textLines = append(textLines, line)
		}
		if artifactResURL != "" {
			textLines = append(textLines, fmt.Sprintf("- result.json: %s", artifactResURL))
		} else if artifactResKey != "" {
			textLines = append(textLines, fmt.Sprintf("- result.json key: `%s`", artifactResKey))
		}

		if len(logBytes) != 0 {
			snippet := logBytes
			snipped := false
			if len(snippet) > 4096 {
				snippet = snippet[len(snippet)-4096:]
				snipped = true
			}
			snippetStr := strings.TrimSpace(string(snippet))
			if snippetStr != "" {
				textLines = append(textLines, "")
				if snipped || logTruncated {
					textLines = append(textLines, "Log tail (redacted; truncated):")
				} else {
					textLines = append(textLines, "Log output (redacted):")
				}
				textLines = append(textLines, "```text", snippetStr, "```")
			}
		}
		text := strings.Join(textLines, "\n")

		if _, err := s.gh.completeCheckRun(ctx, repo, id, conclusion, summary, text); err != nil {
			log.Printf("checkrun reporter update failed repo=%s id=%d: %v", repo, id, err)
			continue
		}

		if s.audit != nil {
			s.audit.Emit("github.checkrun.complete", map[string]any{
				"repo":         repo,
				"check_run_id": id,
				"namespace":    aj.Namespace,
				"agentjob":     aj.Name,
				"conclusion":   conclusion,
			})
		}

		patchBase := aj.DeepCopy()
		if aj.Annotations == nil {
			aj.Annotations = map[string]string{}
		}
		if artifactLogKey != "" {
			aj.Annotations["workspaces.platform.dev/artifact-agent-log-key"] = artifactLogKey
		}
		if artifactLogURL != "" {
			aj.Annotations["workspaces.platform.dev/artifact-agent-log-url"] = artifactLogURL
		}
		if artifactResKey != "" {
			aj.Annotations["workspaces.platform.dev/artifact-result-key"] = artifactResKey
		}
		if artifactResURL != "" {
			aj.Annotations["workspaces.platform.dev/artifact-result-url"] = artifactResURL
		}
		aj.Annotations["workspaces.platform.dev/github-check-run-completed-at"] = time.Now().UTC().Format(time.RFC3339Nano)
		if err := s.k8s.Patch(ctx, aj, ctrlclient.MergeFrom(patchBase)); err != nil {
			log.Printf("checkrun reporter patch agentjob %s/%s: %v", aj.Namespace, aj.Name, err)
		}
	}
}

func (s *server) fetchJobAndPod(ctx context.Context, namespace, jobName string) (*batchv1.Job, *corev1.Pod, error) {
	if s.kube == nil {
		return nil, nil, fmt.Errorf("kube client not configured")
	}
	job, err := s.kube.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	pods, err := s.kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
	if err != nil {
		return job, nil, err
	}
	if len(pods.Items) == 0 {
		return job, nil, nil
	}
	sort.SliceStable(pods.Items, func(i, j int) bool {
		a := pods.Items[i]
		b := pods.Items[j]
		score := func(p corev1.Pod) int {
			switch p.Status.Phase {
			case corev1.PodSucceeded, corev1.PodFailed:
				return 2
			case corev1.PodRunning:
				return 1
			default:
				return 0
			}
		}
		sa, sb := score(a), score(b)
		if sa != sb {
			return sa > sb
		}
		return a.CreationTimestamp.Time.After(b.CreationTimestamp.Time)
	})
	return job, &pods.Items[0], nil
}

func extractContainerExit(pod *corev1.Pod, containerName string) (*int32, string) {
	if pod == nil {
		return nil, ""
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != containerName {
			continue
		}
		if cs.State.Terminated == nil {
			return nil, ""
		}
		ec := int32(cs.State.Terminated.ExitCode)
		reason := strings.TrimSpace(cs.State.Terminated.Reason)
		if reason == "" {
			reason = strings.TrimSpace(cs.State.Terminated.Message)
		}
		return &ec, reason
	}
	return nil, ""
}

func (s *server) fetchPodLogs(ctx context.Context, namespace, podName, containerName string, maxBytes int64) ([]byte, bool, error) {
	if s.kube == nil {
		return nil, false, fmt.Errorf("kube client not configured")
	}
	if strings.TrimSpace(podName) == "" {
		return nil, false, fmt.Errorf("podName is required")
	}
	if maxBytes <= 0 {
		maxBytes = 64 << 10
	}
	opts := &corev1.PodLogOptions{Container: containerName}
	// Let the apiserver do the first cut; still cap client-side to be safe.
	limit := maxBytes
	opts.LimitBytes = &limit

	rc, err := s.kube.CoreV1().Pods(namespace).GetLogs(podName, opts).Stream(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rc.Close() }()

	b, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return nil, false, err
	}
	truncated := int64(len(b)) > maxBytes
	if truncated {
		b = b[:maxBytes]
	}
	if s.redactor != nil {
		b = s.redactor.RedactBytes(b)
	}
	return b, truncated, nil
}
