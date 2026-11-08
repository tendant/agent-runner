package proactive

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// More reasons a proactive run costs its class a level (PROACTIVE_DESIGN.md
// §12): one of its commits is reverted within 7 days, or a person replies in
// its thread that it was wrong. A run demotes its class at most once.

// revertCheckEvery is how often the runs' commits are looked for reverts
// (each look fetches the repos they pushed to).
const revertCheckEvery = 30 * time.Minute

// checkReverts looks, every 30 minutes, for reverts of the commits the last
// 7 days' runs pushed, and demotes the class of a run that was reverted.
func (s *Service) checkReverts(ctx context.Context) {
	if s.cfg.Reverts == nil || s.cfg.Trust == nil {
		return
	}
	s.mu.Lock()
	st := s.load()
	now := s.cfg.Now()
	if now.Sub(st.LastRevertCheck) < revertCheckEvery {
		s.mu.Unlock()
		return
	}
	st.LastRevertCheck = now
	s.save(st)
	type job struct {
		id  string
		run Run
	}
	var jobs []job
	for id, r := range st.Done {
		if !r.Demoted && len(r.Commits) > 0 && now.Sub(r.Started) <= undoWindow {
			jobs = append(jobs, job{id, r})
		}
	}
	s.mu.Unlock()

	for _, j := range jobs {
		byRepo := map[string][]Commit{}
		for _, c := range j.run.Commits {
			byRepo[c.Repo] = append(byRepo[c.Repo], c)
		}
		for repo, commits := range byRepo {
			found, err := s.cfg.Reverts.Reverted(ctx, repo, j.run.Started, commits)
			if err != nil {
				slog.Warn("proactive: could not look for reverts", "repo", repo, "error", err)
				continue
			}
			if len(found) == 0 {
				continue
			}
			rv := found[0]
			why := fmt.Sprintf("commit %s (%q) of session %s was reverted in %s by %s", short(rv.Commit.SHA), rv.Commit.Subject, j.id, repo, short(rv.By))
			s.demoteRun(ctx, j.id, why)
			break
		}
	}
}

// ThreadReply is shown a person's reply in any thread. A reply in a run's
// thread is judged in the background; one that says the run was wrong
// demotes its class. It never consumes the message (handled is false): the
// thread answers it as usual.
func (s *Service) ThreadReply(thread, userID string, admin bool, text string) (string, bool) {
	if thread == "" || s.cfg.Judge == nil || s.cfg.Trust == nil {
		return "", false
	}
	s.mu.Lock()
	st := s.load()
	now := s.cfg.Now()
	var id string
	var run Run
	for _, m := range []map[string]Run{st.Runs, st.Done} {
		for k, r := range m {
			if r.Thread == thread && !r.Demoted && now.Sub(r.Started) <= undoWindow {
				id, run = k, r
			}
		}
	}
	s.mu.Unlock()
	if id == "" {
		return "", false
	}
	go func() {
		wrong, reason, _ := s.cfg.Judge.JudgeReply(context.Background(), run.Work, text)
		if !wrong {
			return
		}
		why := "a person said session " + id + " was wrong"
		if reason != "" {
			why += ": " + reason
		}
		s.demoteRun(context.Background(), id, why)
	}()
	return "", false
}

// demoteRun demotes a run's class once, and tells the chat.
func (s *Service) demoteRun(ctx context.Context, id, why string) {
	s.mu.Lock()
	st := s.load()
	var r Run
	var in map[string]Run
	switch {
	case st.Runs[id].Class != "":
		r, in = st.Runs[id], st.Runs
	case st.Done[id].Class != "":
		r, in = st.Done[id], st.Done
	}
	if in == nil || r.Demoted {
		s.mu.Unlock()
		return
	}
	r.Demoted = true
	in[id] = r
	s.save(st)
	from, to := s.cfg.Trust.Demote(r.Class, why)
	set := s.cfg.Settings()
	s.mu.Unlock()
	s.tell(ctx, set, []string{demotionNotice(r.Class, from, to, why)})
}

func short(sha string) string { return sha[:min(len(sha), 12)] }
