package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
	"github.com/DigitalTolk/ex-runners/internal/taskpolicy"
	"github.com/DigitalTolk/ex-runners/internal/textutil"
	"github.com/DigitalTolk/ex-runners/internal/workspace"
)

// Coding-task tools (plan-coding-agent.md).

// seams over the workspace's git/forge/dev-server plumbing.
var (
	startDevServer     = workspace.StartDevServer
	stopDevServer      = workspace.StopDevServer
	branchHasChanges   = workspace.BranchHasChanges
	pushBranch         = workspace.PushBranch
	createMergeRequest = workspace.CreateMergeRequest
)

func lastSegment(p string) string {
	i := strings.LastIndex(p, "/")
	return p[i+1:]
}

// taskRepoFor resolves a repo reference (full path or its last segment) to
// one of the task's checkouts.
func (s *Server) taskRepoFor(ref string) *taskRepoEnv {
	r := strings.TrimRight(strings.TrimSpace(ref), "/")
	if r == "" {
		return nil
	}
	for i := range s.env.TaskRepos {
		if s.env.TaskRepos[i].Path == r {
			return &s.env.TaskRepos[i]
		}
	}
	for i := range s.env.TaskRepos {
		if lastSegment(s.env.TaskRepos[i].Path) == lastSegment(r) {
			return &s.env.TaskRepos[i]
		}
	}
	return nil
}

func (s *Server) privateDir() string {
	if s.env.PrivateDir != "" {
		return s.env.PrivateDir
	}
	return s.env.WorkDir
}

// allowDevCommand decides a dev-server command the way approval_prompt
// decides a Bash call: the task profile may auto-approve it, so may the
// invoker's standing "always allow shell"; anything else raises a card that
// shows the exact command. publish_test_plan used to start it unseen.
func (s *Server) allowDevCommand(name, cmd string) string {
	d := taskpolicy.BashAllowed(cmd, taskpolicy.Context{TaskDir: s.env.TaskDir, StateDir: s.env.WorkDir})
	if d.Allow && !s.touchesProtected(cmd) {
		return ""
	}
	if s.env.AutoAllow["shell"] && !s.touchesProtected(cmd) {
		return ""
	}
	v := s.awaitApproval(fmt.Sprintf("Start the %s dev server: `%s` — asked because: %s", name, textutil.Clip(cmd, 300), d.Reason), "tool", nil, "shell")
	if v.err != "" {
		return v.err
	}
	if v.state != "approved" {
		note := ""
		if v.note != "" {
			note = " — they say: " + v.note
		}
		return fmt.Sprintf("the requester did not approve starting `%s`%s [retryable=false]", textutil.Clip(cmd, 200), note)
	}
	return ""
}

func (s *Server) workspaceRoot() string {
	if s.env.WorkspaceRoot != "" {
		return s.env.WorkspaceRoot
	}
	return workspace.Root()
}

func (s *Server) createCodingTask(args map[string]any) toolResult {
	project := strings.TrimSpace(str(args, "project"))
	title := strings.TrimSpace(str(args, "title"))
	goal := strings.TrimSpace(str(args, "goal"))
	if project == "" || title == "" || goal == "" {
		return fail("create_coding_task requires project, title and goal")
	}
	payload := map[string]any{"project": project, "title": title, "goal": goal}
	if repos, isList := args["repos"].([]any); isList {
		payload["repos"] = repos
	}
	for _, k := range []string{"kind", "base_branch"} {
		if v := str(args, k); v != "" {
			payload[k] = v
		}
	}
	if t, isObj := args["ticket"].(map[string]any); isObj {
		payload["ticket"] = t
	}
	res := s.callBackend(http.MethodPost, "/api/v1/agent/run/coding-task", payload)
	if !res.ok {
		if res.status == 409 {
			msg := stringOr(res.data["message"], "")
			if stringOr(res.data["error"], "") == "project_unknown" || strings.Contains(msg, "unknown project") {
				if msg == "" {
					msg = "unknown project"
				}
				return fail(msg + " — ask the requester (ask_user or a plain question) which GitLab repositories make up this product and their roles (frontend/backend/…), then call create_coding_task again with repos [retryable=true]")
			}
			if msg == "" {
				msg = "this project already has an active task"
			}
			return fail(msg + " — tell the requester to steer that task in its thread instead of opening another [retryable=false]")
		}
		return s.failure(res)
	}
	return ok(stringOr(res.data["text"], "task created"))
}

func stringList(v any) []string {
	out := []string{}
	if list, isList := v.([]any); isList {
		for _, x := range list {
			if s, isStr := x.(string); isStr {
				out = append(out, s)
			}
		}
	}
	return out
}

func (s *Server) publishTestPlan(args map[string]any) toolResult {
	if s.env.TaskID == "" || s.env.TaskDir == "" {
		return fail("publish_test_plan only works inside a coding task run")
	}
	extra := ""
	servers, _ := args["servers"].([]any)
	for _, raw := range servers {
		sv, _ := raw.(map[string]any)
		cmd := strings.TrimSpace(str(sv, "cmd"))
		if cmd == "" {
			continue
		}
		repo := s.taskRepoFor(str(sv, "repo"))
		cwd, name := s.env.TaskDir, "server"
		if repo != nil {
			cwd, name = repo.Dir, lastSegment(repo.Path)
		}
		if why := s.allowDevCommand(name, cmd); why != "" {
			return fail(why)
		}
		h, err := startDevServer(context.Background(), cmd, cwd, s.env.WorkDir, s.privateDir(), str(sv, "url"), protocol.NopLogger, name)
		if err != nil {
			return fail(fmt.Sprintf("dev server for %s failed: %s — fix it and call publish_test_plan again", name, err.Error()))
		}
		extra += fmt.Sprintf(" %s: running (pid %d, log %s).", name, h.PID, h.LogFile)
		if repo != nil {
			_, _ = workspace.UpdateProjectCommands(s.workspaceRoot(), repo.Path, workspace.CommandUpdate{DevCmd: &cmd})
		}
	}
	res := s.callBackend(http.MethodPost, "/api/v1/agent/run/coding-task/test-plan", map[string]any{
		"url":           strings.TrimSpace(str(args, "url")),
		"steps":         stringList(args["steps"]),
		"counter_steps": stringList(args["counter_steps"]),
		"accounts":      str(args, "accounts"),
		"notes":         str(args, "notes"),
	})
	if !res.ok {
		if res.status == 400 {
			return fail(stringOr(res.data["message"], "invalid test plan") + " — fix the plan and call publish_test_plan again")
		}
		return s.failure(res)
	}
	return ok(stringOr(res.data["text"], "published") + extra)
}

type mrOutcome struct {
	path, url, err string
	changed        bool
	existed        bool
}

func (s *Server) requestMR(args map[string]any) toolResult {
	if s.env.TaskID == "" || s.env.TaskDir == "" {
		return fail("request_mr only works inside a coding task run")
	}
	res := s.callBackend(http.MethodPost, "/api/v1/agent/run/coding-task/request-mr", map[string]any{})
	if !res.ok {
		return s.failure(res)
	}
	status := stringOr(res.data["status"], "")
	if status == "ask" {
		// The gate: the requester decides, on their own approval card. The
		// backend raises it and returns its id (see use_connector).
		summary := stringOr(res.data["summary"], "Create the merge request(s) for this task")
		var v verdict
		if gate, isStr := res.data["approvalID"].(string); isStr && gate != "" {
			v = s.pollApproval(gate, time.Time{})
		} else {
			v = s.awaitApproval(summary, "high", nil, "")
		}
		if v.err != "" {
			return fail(v.err)
		}
		if v.state != "approved" {
			note := ""
			if v.note != "" {
				note = " — they say: " + v.note
			}
			return fail("the requester did not sign off" + note + " — do NOT push; act on their feedback in the thread [retryable=false]")
		}
		res = s.callBackend(http.MethodPost, "/api/v1/agent/run/coding-task/request-mr", map[string]any{"approvalID": v.approvalID})
		if !res.ok {
			return s.failure(res)
		}
		status = stringOr(res.data["status"], "")
	}
	if status != "approved" {
		return fail(stringOr(res.data["message"], status) + " [retryable=false]")
	}
	cred, has := s.connector("gitlab")
	if !has || cred.Token == "" {
		return fail("no GitLab connector is attached to this run — the requester must install the gitlab connector (Connectors page) with a token that has api + write_repository scopes, then reply here to retry [retryable=false]")
	}
	if len(s.env.TaskRepos) == 0 {
		return fail("no repo checkouts are known for this run — report the workspace state first and retry")
	}
	host := workspace.GitHostFromBaseURL(cred.BaseURL)
	gc := workspace.GitCred{Host: host, Token: cred.Token}
	labels := []string{"ex:dev"}
	if l, isList := res.data["labels"].([]any); isList {
		labels = stringList(l)
	}
	// MR body: the agent's reviewer-facing note for the repo (repo_notes >
	// summary), else the server's short fallback — plus the Ex footer.
	notes, _ := args["repo_notes"].([]any)
	noteFor := func(repoPath string) string {
		for _, raw := range notes {
			n, _ := raw.(map[string]any)
			repo, note := str(n, "repo"), strings.TrimSpace(str(n, "note"))
			if repo == "" || note == "" {
				continue
			}
			if repo == repoPath || strings.HasSuffix(repoPath, "/"+repo) || repo == lastSegment(repoPath) {
				return note
			}
		}
		return ""
	}
	summaryArg := strings.TrimSpace(str(args, "summary"))
	footer, _ := res.data["mrFooter"].(string)
	fallback, _ := res.data["mrBodyFallback"].(string)
	mrBody := func(repoPath string) string {
		base := noteFor(repoPath)
		if base == "" {
			base = summaryArg
		}
		if base == "" {
			base = fallback
		}
		if footer != "" {
			return base + "\n\n---\n" + footer
		}
		if base != "" {
			return base
		}
		return stringOr(res.data["mrDescription"], "")
	}
	ctx := context.Background()
	var results []mrOutcome
	for _, r := range s.env.TaskRepos {
		changed, err := branchHasChanges(ctx, r.Dir, r.Base, &gc)
		if err == nil && !changed {
			results = append(results, mrOutcome{path: r.Path})
			continue
		}
		if err == nil {
			_, err = pushBranch(ctx, r.Dir, r.Path, r.Branch, gc, protocol.NopLogger)
		}
		var mr workspace.MergeRequestResult
		if err == nil {
			mr, err = createMergeRequest(ctx, workspace.MergeRequestInput{
				Host:         host,
				APIBase:      cred.BaseURL,
				Token:        cred.Token,
				ProjectPath:  r.Path,
				SourceBranch: r.Branch,
				TargetBranch: r.Base,
				Title:        stringOr(res.data["mrTitle"], r.Branch),
				Description:  mrBody(r.Path),
				Labels:       labels,
			})
		}
		if err != nil {
			results = append(results, mrOutcome{path: r.Path, changed: true, err: err.Error()})
			continue
		}
		results = append(results, mrOutcome{path: r.Path, url: mr.URL, changed: true, existed: mr.Existed})
	}
	var opened, failed []mrOutcome
	for _, x := range results {
		if x.url != "" {
			opened = append(opened, x)
		}
		if x.err != "" {
			failed = append(failed, x)
		}
	}
	if len(opened) == 0 {
		why := "no repo has commits beyond its base — commit your work first"
		if len(failed) > 0 {
			parts := []string{}
			for _, f := range failed {
				parts = append(parts, lastSegment(f.path)+": "+f.err)
			}
			why = strings.Join(parts, "; ")
		}
		return fail("push/MR failed: " + why + " — fix the cause and call request_mr again")
	}
	allExisted := true
	links, summaries, reported := []string{}, []string{}, []map[string]any{}
	for _, x := range opened {
		allExisted = allExisted && x.existed
		links = append(links, lastSegment(x.path)+" → "+x.url)
		summaries = append(summaries, x.path+": "+x.url)
		reported = append(reported, map[string]any{"path": x.path, "mr_url": x.url, "changed": true})
	}
	plural := "s"
	if len(opened) == 1 {
		plural = ""
	}
	verb := "created"
	if allExisted {
		verb = "already open"
	}
	note := fmt.Sprintf("🔀 Merge request%s %s: %s", plural, verb, strings.Join(links, " · "))
	if len(failed) > 0 {
		names := []string{}
		for _, f := range failed {
			names = append(names, lastSegment(f.path))
		}
		note += " — ⚠️ failed for " + strings.Join(names, ", ")
	}
	rep := s.callBackend(http.MethodPost, "/api/v1/agent/run/coding-task/report", map[string]any{"state": "mr_created", "repos": reported, "note": note})
	stopDevServer(s.privateDir(), protocol.NopLogger, "")
	summaryText := strings.Join(summaries, "; ")
	if !rep.ok {
		return ok(fmt.Sprintf("merge request(s) opened (%s), but recording them failed: %s. Post the links in the thread.", summaryText, describeFailure(rep.status, rep.data)))
	}
	found := "created"
	if allExisted {
		found = "found"
	}
	out := fmt.Sprintf("merge request%s %s: %s", plural, found, summaryText)
	if len(failed) > 0 {
		parts := []string{}
		for _, f := range failed {
			parts = append(parts, f.path+": "+f.err)
		}
		out += " — FAILED for " + strings.Join(parts, "; ") + " (fix and call request_mr again for those)"
	}
	return ok(out + " — the task is now mr_created and the thread has been told. Post a one-line wrap-up (what changed, the MR links) and end your turn.")
}

func (s *Server) registerProjectCommands(args map[string]any) toolResult {
	if s.env.TaskID == "" || s.env.TaskProject == "" {
		return fail("register_project_commands only works inside a coding task run")
	}
	repo := s.taskRepoFor(str(args, "repo"))
	if repo == nil && len(s.env.TaskRepos) == 1 {
		repo = &s.env.TaskRepos[0]
	}
	if repo == nil {
		paths := []string{}
		for _, r := range s.env.TaskRepos {
			paths = append(paths, r.Path)
		}
		known := strings.Join(paths, ", ")
		if known == "" {
			known = "(none known)"
		}
		return fail("register_project_commands needs repo — one of: " + known)
	}
	opt := func(k string) *string {
		if v, isStr := args[k].(string); isStr {
			return &v
		}
		return nil
	}
	upd := workspace.CommandUpdate{SetupCmd: opt("setup_cmd"), TestCmd: opt("test_cmd"), DevCmd: opt("dev_cmd"), Notes: opt("notes")}
	if n, isNum := num(args, "port"); isNum {
		p := int(n)
		upd.Port = &p
	}
	next, err := workspace.UpdateProjectCommands(s.workspaceRoot(), repo.Path, upd)
	if err != nil {
		return fail("registry update failed: " + err.Error())
	}
	summary := map[string]any{}
	if next.SetupCmd != "" {
		summary["setup"] = next.SetupCmd
	}
	if next.TestCmd != "" {
		summary["test"] = next.TestCmd
	}
	if next.DevCmd != "" {
		summary["dev"] = next.DevCmd
	}
	if next.Port != 0 {
		summary["port"] = next.Port
	}
	raw, _ := json.Marshal(orderedSummary(summary))
	return ok(fmt.Sprintf("registry updated for %s: %s", repo.Path, raw))
}

// orderedSummary keeps the TS key order (setup, test, dev, port) in the
// registry echo; encoding/json would sort a map.
func orderedSummary(m map[string]any) json.RawMessage {
	parts := []string{}
	for _, k := range []string{"setup", "test", "dev", "port"} {
		if v, has := m[k]; has {
			kb, _ := json.Marshal(k)
			vb, _ := json.Marshal(v)
			parts = append(parts, string(kb)+":"+string(vb))
		}
	}
	return json.RawMessage("{" + strings.Join(parts, ",") + "}")
}
