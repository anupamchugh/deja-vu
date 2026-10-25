package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vshulcz/deja-vu/internal/index"
	"github.com/vshulcz/deja-vu/internal/model"
	"github.com/vshulcz/deja-vu/internal/policy"
	"github.com/vshulcz/deja-vu/internal/redact"
	"github.com/vshulcz/deja-vu/internal/search"
	"github.com/vshulcz/deja-vu/internal/sources"
)

// `deja repo` is project memory committed in the repository: a few picked
// decisions, dropped approaches and traps in <root>/.deja/memory.jsonl, which
// everyone who clones the repository gets in recall. deja proposes and writes
// entries only when asked; the person reviews the diff and commits it.

var (
	repoFlags    = []string{"--json"}
	repoAddFlags = []string{"--kind"}
)

const repoFileShown = sources.RepoMemoryDir + "/" + sources.RepoMemoryName

func runRepo(dir string, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "candidates":
			return runRepoCandidates(dir, os.Stdout, args[1:])
		case "add":
			return runRepoAdd(dir, os.Stdout, args[1:])
		}
	}
	return runRepoList(os.Stdout, args)
}

// repoCheckout is the repository the command is about: the one the process
// stands in.
func repoCheckout() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root := sources.RepoRoot(cwd)
	if root == "" {
		return "", errors.New("repo: not inside a git repository — repo memory lives in the repository's .deja/ directory")
	}
	return root, nil
}

func runRepoList(w io.Writer, args []string) error {
	asJSON := false
	for _, a := range args {
		if a != "--json" {
			return unknownFlag("repo", a, repoFlags)
		}
		asJSON = true
	}
	root, err := repoCheckout()
	if err != nil {
		return err
	}
	path := sources.RepoMemoryPath(root)
	entries, bad, err := sources.ReadRepoMemory(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sources.RememberRepoCheckout(root)
	if asJSON {
		if entries == nil {
			entries = []sources.RepoEntry{}
		}
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		return enc.Encode(entries)
	}
	if len(entries) == 0 && bad == 0 {
		fmt.Fprintf(w, "no %s in this repository yet — `deja repo candidates` proposes entries from your history\n", repoFileShown)
		return nil
	}
	fmt.Fprintf(w, "%d entr%s in %s\n", len(entries), pluralY(len(entries)), repoFileShown)
	for _, e := range entries {
		fmt.Fprintln(w, repoEntryRow(e))
	}
	if bad > 0 {
		fmt.Fprintf(w, "%d line%s deja cannot read — a merge conflict or a hand edit; fix or delete %s before adding more\n", bad, pluralS(bad), pluralThem(bad))
	}
	if line := repoPolicyLine(sources.RepoMemoryProject(root)); line != "" {
		fmt.Fprintln(w, line)
	}
	return nil
}

func repoEntryRow(e sources.RepoEntry) string {
	from := ""
	if e.From != "" {
		from = "  (" + search.SafeLine(e.From) + ")"
	}
	return fmt.Sprintf("%s %-8s %s%s", search.SafeLine(e.ID), e.Kind, search.SafeLine(e.Text), from)
}

// repoPolicyLine says when the trust policy keeps these entries off a path, so
// a list that shows entries is not read as "the agent gets these".
func repoPolicyLine(project string) string {
	pol := policy.Load()
	var off []string
	for _, a := range []string{policy.ActivationSearch, policy.ActivationMCP, policy.ActivationAuto} {
		if !pol.Allows(a, project) {
			off = append(off, a)
		}
	}
	if len(off) == 0 {
		return ""
	}
	return "your policy keeps repo memory out of " + strings.Join(off, ", ") + " (" + policy.Path() + ")"
}

func runRepoAdd(dir string, w io.Writer, args []string) error {
	kind := ""
	var words []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--kind":
			if i+1 >= len(args) {
				return errors.New("repo add: --kind needs decision, rejected or gotcha")
			}
			i++
			kind = strings.ToLower(args[i])
		case strings.HasPrefix(a, "--kind="):
			kind = strings.ToLower(strings.TrimPrefix(a, "--kind="))
		case a == "--":
			words = append(words, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "-"):
			return unknownFlag("repo add", a, repoAddFlags)
		default:
			words = append(words, a)
		}
	}
	if len(words) == 0 {
		return errors.New(`repo add: name candidate ids from ` + "`deja repo candidates`" + `, or write one: deja repo add --kind rejected "the text"`)
	}
	root, err := repoCheckout()
	if err != nil {
		return err
	}
	var entries []sources.RepoEntry
	if kind != "" {
		if !sources.RepoKinds[kind] {
			return fmt.Errorf("repo add: --kind is decision, rejected or gotcha, got %q", kind)
		}
		entries = append(entries, sources.RepoEntry{Kind: kind, Text: strings.Join(words, " ")})
	} else {
		picked, err := pickRepoCandidates(dir, root, words)
		if err != nil {
			return err
		}
		entries = picked
	}
	return writeRepoEntries(w, root, entries)
}

// pickRepoCandidates resolves ids from the candidates list. Ids are hashes of
// the entry, so the list does not have to be the same length or order as the
// one the person read.
func pickRepoCandidates(dir, root string, ids []string) ([]sources.RepoEntry, error) {
	if err := index.Ensure(dir, "", false, noteIndexNarration(dir)); err != nil {
		return nil, ensureError(dir, err)
	}
	cands, err := collectRepoCandidates(dir, root, 0, 0)
	if err != nil {
		return nil, err
	}
	byID := map[string]repoCandidate{}
	for _, c := range cands {
		byID[c.ID] = c
	}
	var out []sources.RepoEntry
	for _, id := range ids {
		id = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(id)), "#")
		c, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("repo add: %q is not a candidate id — `deja repo candidates` lists them, and --kind writes your own text", search.SafeLine(id))
		}
		if c.Refused != "" {
			return nil, fmt.Errorf("repo add: %s is refused: %s", id, c.Refused)
		}
		out = append(out, sources.RepoEntry{ID: c.ID, Kind: c.Kind, Text: c.Text, From: c.From})
	}
	return out, nil
}

// writeRepoEntries redacts each entry, refuses one that still holds a secret,
// writes the rest and says what to do next. Nothing is staged.
func writeRepoEntries(w io.Writer, root string, entries []sources.RepoEntry) error {
	masked := 0
	for i := range entries {
		text, n, err := repoSafeText(entries[i].Text)
		if err != nil {
			return fmt.Errorf("repo add: %w", err)
		}
		masked += n
		entries[i].Text = text
		entries[i].ID = sources.RepoEntryID(entries[i].Kind, text)
		if entries[i].Date == "" {
			entries[i].Date = time.Now().UTC().Format("2006-01-02")
		}
	}
	dirBefore := fileThere(filepath.Join(root, sources.RepoMemoryDir))
	added, err := sources.AddRepoEntries(root, entries)
	if errors.Is(err, sources.ErrRepoMemoryMalformed) {
		return fmt.Errorf("repo add: %s has a line deja cannot read (a merge conflict or a hand edit) — fix or delete it first; `deja repo` counts them", repoFileShown)
	}
	if err != nil {
		return fmt.Errorf("repo add: %w", err)
	}
	if len(added) == 0 {
		fmt.Fprintf(w, "already in %s\n", repoFileShown)
		return nil
	}
	fmt.Fprintf(w, "added %d entr%s to %s\n", len(added), pluralY(len(added)), repoFileShown)
	for _, e := range added {
		fmt.Fprintln(w, "  "+repoEntryRow(e))
	}
	if !dirBefore {
		fmt.Fprintf(w, "wrote %s/.gitattributes so parallel additions merge line by line, and a short %s/README.md for whoever opens the folder\n", sources.RepoMemoryDir, sources.RepoMemoryDir)
	}
	if masked > 0 {
		fmt.Fprintf(w, "%d secret%s or private detail%s masked. pattern redaction is a floor — read the diff before you commit\n", masked, pluralS(masked), pluralS(masked))
	}
	fmt.Fprintf(w, "nothing is committed: review with `git diff %s/` and commit it like code\n", sources.RepoMemoryDir)
	return nil
}

func fileThere(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// repoSafeText is an entry as it may be committed: the secret rules the
// index and every export use, then the outbound pass for home paths, emails,
// internal hosts and private addresses. It is read once more afterwards, and
// text that still matches a secret rule is refused rather than written.
func repoSafeText(text string) (string, int, error) {
	if redact.Disabled() {
		return "", 0, errors.New("redaction is off (DEJA_NO_REDACT=1), and nothing is written into a repository without it")
	}
	clean := sources.CleanRepoText(text)
	if clean == "" {
		return "", 0, errors.New("the entry is empty")
	}
	out, secrets := redact.Text(clean)
	out, private := redact.Outbound(out)
	out = sources.CleanRepoText(out)
	if _, left := redact.Text(out); len(left) > 0 {
		return "", 0, fmt.Errorf("the entry still matches a secret rule (%s) after redaction — write it without the value", strings.Join(sortedKinds(left), ", "))
	}
	if n := len([]rune(out)); n > sources.RepoTextMax {
		return "", 0, fmt.Errorf("the entry is %d characters, over %d — shorten it and add it with --kind", n, sources.RepoTextMax)
	}
	return out, countAll(secrets) + countAll(private), nil
}

func countAll(c redact.Counts) int {
	n := 0
	for _, v := range c {
		n += v
	}
	return n
}

func sortedKinds(c redact.Counts) []string {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// repoRememberKind reads the MCP remember tool's repo target: "repo:" alone
// files a decision, "repo:rejected" and "repo:gotcha" the other kinds.
func repoRememberKind(project string) (string, bool, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(project), model.RepoProjectPrefix)
	if !ok {
		return "", false, nil
	}
	kind := strings.ToLower(strings.TrimSpace(rest))
	if kind == "" {
		kind = "decision"
	}
	if !sources.RepoKinds[kind] {
		return "", true, argErrorf("project %q: after repo: comes decision, rejected or gotcha", search.SafeLine(project))
	}
	return kind, true, nil
}

// mcpRememberRepo writes one entry for the MCP remember tool. The agent was
// asked to put it in the repository; the reply says it is not committed.
func mcpRememberRepo(kind, text string) (string, error) {
	root, err := repoCheckout()
	if err != nil {
		return "", argErrorf("%v", err)
	}
	var b strings.Builder
	if err := writeRepoEntries(&b, root, []sources.RepoEntry{{Kind: kind, Text: text}}); err != nil {
		return "", argErrorf("%v", err)
	}
	return strings.TrimSpace(b.String()) + "\nTell the user it is written but not committed, and to review the diff.", nil
}

// repoMemoryBlock is what the session-start digest says about the
// repository's own entries: read from the file rather than the index, so a
// fresh clone has them before deja has indexed anything there. Dropped
// approaches and traps lead, since they are what an agent repeats.
func repoMemoryBlock(cwd string, pol policy.Policy) string {
	const maxRows, budget = 8, 900
	root := sources.RepoRoot(cwd)
	if root == "" {
		return ""
	}
	if !pol.Allows(policy.ActivationAuto, sources.RepoMemoryProject(root)) {
		return ""
	}
	entries, _, err := sources.ReadRepoMemory(sources.RepoMemoryPath(root))
	if err != nil || len(entries) == 0 {
		return ""
	}
	sources.RememberRepoCheckout(root)
	rank := map[string]int{"rejected": 0, "gotcha": 1, "decision": 2}
	sort.SliceStable(entries, func(i, j int) bool {
		if rank[entries[i].Kind] != rank[entries[j].Kind] {
			return rank[entries[i].Kind] < rank[entries[j].Kind]
		}
		return entries[i].Date > entries[j].Date
	})
	var b strings.Builder
	b.WriteString("from this repository's " + repoFileShown + " (committed by the people working on it):\n")
	head := b.Len()
	for i, e := range entries {
		if i >= maxRows {
			break
		}
		row := "  · " + search.SafeLine(sources.RepoEntryBody(e)) + "\n"
		if b.Len()+len(row) > budget {
			break
		}
		b.WriteString(row)
	}
	if b.Len() == head {
		return ""
	}
	if more := len(entries) - strings.Count(b.String()[head:], "\n"); more > 0 {
		b.WriteString("  " + strconv.Itoa(more) + " more — recall finds them by topic\n")
	}
	return b.String()
}

// joinBlocks puts two newline-ended blocks one after the other.
func joinBlocks(a, b string) string {
	if a == "" {
		return b
	}
	return strings.TrimRight(a, "\n") + "\n" + b
}

// repoMemoryStamp identifies the checkout's file for the session-start cache,
// so an entry added or deleted shows at the next start rather than after the
// cache runs out. Empty when there is no file, which leaves the gate as it was.
func repoMemoryStamp(cwd string) string {
	root := sources.RepoRoot(cwd)
	if root == "" {
		return ""
	}
	fi, err := os.Stat(sources.RepoMemoryPath(root))
	if err != nil {
		return ""
	}
	return "|repo:" + strconv.FormatInt(fi.Size(), 10) + "@" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)
}
