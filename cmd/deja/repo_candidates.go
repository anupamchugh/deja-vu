package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vshulcz/deja-vu/internal/digest"
	"github.com/vshulcz/deja-vu/internal/index"
	"github.com/vshulcz/deja-vu/internal/model"
	"github.com/vshulcz/deja-vu/internal/policy"
	"github.com/vshulcz/deja-vu/internal/search"
	"github.com/vshulcz/deja-vu/internal/sources"
)

// `deja repo candidates` proposes entries for the repository from this
// project's history and writes nothing. Three sources, best first: notes the
// person promoted (accepted is a decision, rejected stays rejected), notes they
// remembered, and what sessions said, read with the detectors deja already
// runs: a reversal ("reverted the cache, it broke ordering") is a dropped
// approach, a session's settled conclusion a decision. The last two are
// guesses, which is why the person picks.

var repoCandidateFlags = []string{"--json", "--limit", "--since"}

type repoCandidate struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	From    string `json:"from,omitempty"`
	Source  string `json:"source"`
	Session string `json:"session,omitempty"`
	InFile  bool   `json:"in_file,omitempty"`
	Refused string `json:"refused,omitempty"`
	at      time.Time
}

func runRepoCandidates(dir string, w io.Writer, args []string) error {
	asJSON := false
	limit := 30
	var since time.Duration
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
		case "--limit":
			if i+1 >= len(args) {
				return errors.New("repo candidates: --limit needs value")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n <= 0 {
				return fmt.Errorf("repo candidates: --limit wants a positive number, got %q", args[i])
			}
			limit = n
		case "--since":
			if i+1 >= len(args) {
				return errors.New("repo candidates: --since needs value")
			}
			i++
			d, err := parseDur(args[i])
			if err != nil {
				return err
			}
			since = d
		default:
			return unknownFlag("repo candidates", args[i], repoCandidateFlags)
		}
	}
	root, err := repoCheckout()
	if err != nil {
		return err
	}
	if err = index.Ensure(dir, "", false, os.Stderr); err != nil {
		return ensureError(dir, err)
	}
	cands, err := collectRepoCandidates(dir, root, since, limit)
	if err != nil {
		return err
	}
	if asJSON {
		if cands == nil {
			cands = []repoCandidate{}
		}
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		return enc.Encode(cands)
	}
	name := sources.ClaudeProjectName(root)
	if len(cands) == 0 {
		fmt.Fprintf(w, "no candidates for %s in your history — `deja repo add --kind decision \"...\"` writes one by hand\n", search.SafeLine(name))
		return nil
	}
	fmt.Fprintf(w, "%d candidate%s for %s, newest first. nothing is written until you pick\n", len(cands), pluralS(len(cands)), search.SafeLine(name))
	for _, c := range cands {
		mark := ""
		switch {
		case c.InFile:
			mark = "  [already in " + repoFileShown + "]"
		case c.Refused != "":
			mark = "  [refused: " + c.Refused + "]"
		}
		from := c.Source
		if c.From != "" {
			from += ", " + c.From
		}
		fmt.Fprintf(w, "%s %-8s %s  (%s)%s\n", c.ID, c.Kind, search.SafeLine(c.Text), search.SafeLine(from), mark)
	}
	fmt.Fprintln(w, "add the ones that hold for everyone: deja repo add <id> [<id>...]")
	return nil
}

// collectRepoCandidates gathers candidates for the project root is, newest
// first, with limit keeping the newest; since and limit of zero mean all.
func collectRepoCandidates(dir, root string, since time.Duration, limit int) ([]repoCandidate, error) {
	names := digest.ProjectNameCandidates(root)
	inScope := func(project string) bool {
		if strings.HasPrefix(project, "repo:") {
			return false
		}
		for _, n := range names {
			if n != "" && index.ProjectInScope(project, n) {
				return true
			}
		}
		return false
	}
	pol := policy.Load()
	allowed := func(path, project string) bool {
		return pol.Allows(policy.ActivationSearch, project) && !pol.Ignored(path, project) && !sources.ExcludedProject(project)
	}
	var out []repoCandidate
	add := func(c repoCandidate) {
		text, _, err := repoSafeText(c.Text)
		if err != nil {
			c.Refused = err.Error()
			c.Text = search.SafeLine(squeezeTrim(c.Text, 120))
		} else {
			c.Text = text
		}
		c.ID = sources.RepoEntryID(c.Kind, c.Text)
		out = append(out, c)
	}
	for _, n := range sources.LoadPromotedNotes() {
		if !inScope(n.Project) || !allowed("", n.Project) {
			continue
		}
		var kind string
		switch n.State {
		case "accepted":
			kind = "decision"
		case "rejected":
			kind = "rejected"
		default:
			continue
		}
		text := strings.TrimSpace(n.Text)
		if text == "" {
			text = n.Title
		}
		harness, _, _ := strings.Cut(n.Session, ":")
		add(repoCandidate{Kind: kind, Text: text, From: repoFrom(harness, n.At), Source: "promoted note", at: n.At})
	}
	for _, s := range sources.LoadNotes() {
		if strings.HasPrefix(s.ID, "deja-note-") || !inScope(s.Project) || !allowed(s.Path, s.Project) {
			continue
		}
		for _, m := range s.Messages {
			add(repoCandidate{Kind: "decision", Text: m.Text, From: repoFrom("deja", m.Time), Source: "your note", at: m.Time})
		}
	}
	if err := sessionRepoCandidates(dir, inScope, allowed, add); err != nil {
		return nil, err
	}
	var cutoff time.Time
	if since > 0 {
		cutoff = time.Now().Add(-since)
	}
	in := map[string]bool{}
	if entries, _, err := sources.ReadRepoMemory(sources.RepoMemoryPath(root)); err == nil {
		for _, e := range entries {
			in[e.ID] = true
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.After(out[j].at) })
	seen := map[string]bool{}
	kept := out[:0]
	for _, c := range out {
		if seen[c.ID] || (!cutoff.IsZero() && c.at.Before(cutoff)) {
			continue
		}
		seen[c.ID] = true
		c.InFile = in[c.ID]
		kept = append(kept, c)
	}
	if limit > 0 && len(kept) > limit {
		kept = kept[:limit]
	}
	return kept, nil
}

// sessionRepoCandidates reads this project's sessions: each reversal line said
// by the user or the agent, and each session's settled conclusion when it
// carries a decision.
func sessionRepoCandidates(dir string, inScope func(string) bool, allowed func(string, string) bool, add func(repoCandidate)) error {
	metas, err := index.AllMeta(dir)
	if err != nil {
		return err
	}
	pick := map[string]index.SessionMeta{}
	for _, m := range metas {
		if m.Harness == "deja" || m.Kind != "" || sources.IsSubagentPath(m.Path) || !inScope(m.Project) || !allowed(m.Path, m.Project) {
			continue
		}
		key := m.Harness + ":" + m.ID
		pick[key] = m
		if d := strings.TrimSpace(m.Settled); d != "" && digest.CarriesDecision(d) {
			add(repoCandidate{Kind: "decision", Text: squeezeTrim(d, 300), From: repoFrom(m.Harness, m.Updated), Source: "session " + shortID(m.ID), Session: m.ID, at: m.Updated})
		}
	}
	if len(pick) == 0 {
		return nil
	}
	// The agent's turns, kept per session for the conclusion read below; only
	// this project's sessions, so the walk holds little whatever the store is.
	said := map[string]*model.Session{}
	err = index.EachRecordInRoles(dir, []string{"user", "assistant"}, func(meta index.SessionMeta, r index.Record) {
		key := meta.Harness + ":" + meta.ID
		if _, ok := pick[key]; !ok {
			return
		}
		if r.Role == "assistant" {
			s := said[key]
			if s == nil {
				s = &model.Session{ID: meta.ID, Harness: meta.Harness, Project: meta.Project}
				said[key] = s
			}
			s.Messages = append(s.Messages, model.Message{Role: r.Role, Text: r.Text, Time: r.Time})
		}
		if !meta.GaveUp {
			return
		}
		for line := range strings.SplitSeq(r.Text, "\n") {
			l, ok := index.GiveUpLine(line)
			if !ok || strings.Contains(strings.ToLower(l), "deja") {
				continue
			}
			add(repoCandidate{Kind: "rejected", Text: strings.Trim(l, "*-> \t"), From: repoFrom(meta.Harness, r.Time), Source: "session " + shortID(meta.ID), Session: meta.ID, at: r.Time})
		}
	})
	if err != nil {
		return err
	}
	for key, s := range said {
		m := pick[key]
		for _, d := range digest.SubstantialConclusions(*s, promoteDecisionBudget, 2) {
			if d = strings.TrimSpace(d); d != "" && digest.CarriesDecision(d) {
				add(repoCandidate{Kind: "decision", Text: squeezeTrim(d, 300), From: repoFrom(m.Harness, m.Updated), Source: "session " + shortID(m.ID), Session: m.ID, at: m.Updated})
			}
		}
	}
	return nil
}

// repoFrom is the provenance an entry carries into the repository: the agent
// and the day, which a teammate can read, and nothing that names this machine.
func repoFrom(harness string, t time.Time) string {
	harness = strings.TrimSpace(harness)
	if t.IsZero() {
		return harness
	}
	day := t.UTC().Format("2006-01-02")
	if harness == "" {
		return day
	}
	return harness + " " + day
}
