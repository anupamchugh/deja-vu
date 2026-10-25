package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/vshulcz/deja-vu/internal/sources"
)

// repoPerson points every store deja reads and writes at one person's temp
// home. Two of them on one test stand are two machines.
func repoPerson(t *testing.T, base, who string) string {
	t.Helper()
	home := filepath.Join(base, who)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("DEJA_INDEX_DIR", filepath.Join(home, "index"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("DEJA_CLAUDE_ROOT", filepath.Join(home, "claude"))
	t.Setenv("HERMES_HOME", filepath.Join(home, "hermes"))
	t.Setenv("DEJA_NOTES_FILE", "")
	return home
}

func repoGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main", "-c", "pull.rebase=false"}, args...)
	cmd := exec.CommandContext(t.Context(), "git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func repoStandBase(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return base
}

// One person writes an entry into a repository, a second clones it and gets
// the entry in recall and at session start; parallel additions merge.
func TestRepoMemoryTravelsWithAClone(t *testing.T) {
	base := repoStandBase(t)
	origin := filepath.Join(base, "origin.git")
	repoGit(t, base, "init", "-q", "--bare", origin)

	repoPerson(t, base, "alice")
	alice := filepath.Join(base, "alice", "src", "webhooks")
	repoGit(t, base, "clone", "-q", origin, alice)
	t.Chdir(alice)
	if _, err := captureRun(t, "remember", "the outbox table owns webhook retries"); err != nil {
		t.Fatal(err)
	}
	out, err := captureRun(t, "repo", "candidates", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var cands []repoCandidate
	if err = json.Unmarshal([]byte(out), &cands); err != nil || len(cands) == 0 {
		t.Fatalf("candidates %q: %v", out, err)
	}
	if cands[0].Kind != "decision" || !strings.Contains(cands[0].Text, "outbox table") {
		t.Fatalf("candidate = %+v", cands[0])
	}
	if out, err = captureRun(t, "repo", "add", cands[0].ID); err != nil || !strings.Contains(out, "added 1 entry") {
		t.Fatalf("add: %q %v", out, err)
	}
	if out, err = captureRun(t, "repo", "add", "--kind", "rejected", "retry with jitter in the webhook sender broke idempotency"); err != nil {
		t.Fatalf("add --kind: %q %v", out, err)
	}
	if !strings.Contains(out, "nothing is committed") {
		t.Errorf("add did not say it commits nothing: %q", out)
	}
	if st := repoGit(t, alice, "status", "--porcelain"); !strings.Contains(st, ".deja/") {
		t.Fatalf("nothing to review: %q", st)
	}
	b, _ := os.ReadFile(sources.RepoMemoryPath(alice))
	for _, leak := range []string{base, "alice/src", "claude:"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("file holds %q:\n%s", leak, b)
		}
	}
	repoGit(t, alice, "add", ".deja")
	repoGit(t, alice, "commit", "-qm", "repo memory")
	repoGit(t, alice, "push", "-q", "origin", "main")

	repoPerson(t, base, "bob")
	bob := filepath.Join(base, "bob", "code", "webhooks")
	repoGit(t, base, "clone", "-q", origin, bob)
	if _, err = os.Stat(filepath.Join(bob, ".deja", "README.md")); err != nil {
		t.Errorf("the clone has no .deja/README.md: %v", err)
	}
	t.Chdir(bob)
	out, err = captureRun(t, "recall", "retry jitter webhook")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[rejected] retry with jitter in the webhook sender broke idempotency") || !strings.Contains(out, "repo:") {
		t.Fatalf("bob's recall misses the entry:\n%s", out)
	}
	if !strings.HasPrefix(out, recallFrameHeader) {
		t.Errorf("recall is not framed:\n%s", out)
	}
	if strings.Contains(out, "abandoned one approach") {
		t.Errorf("a rejected entry read as an abandoned session:\n%s", out)
	}
	dir := os.Getenv("DEJA_INDEX_DIR")
	digest := hookDigest(dir)
	if !strings.Contains(digest, "from this repository's .deja/memory.jsonl") || !strings.Contains(digest, "[rejected] retry with jitter") {
		t.Fatalf("session start misses the entry:\n%s", digest)
	}
	if i, j := strings.Index(digest, "[rejected]"), strings.Index(digest, "[decision]"); i < 0 || j < 0 || i > j {
		t.Errorf("rejected should lead the block:\n%s", digest)
	}

	// The trust policy keeps it out per path.
	pol := filepath.Join(base, "bob", "config", "deja", "policy.json")
	if err = os.MkdirAll(filepath.Dir(pol), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(pol, []byte(`{"activations":{"auto":{"repo":false},"mcp":{"repo":false}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := hookDigest(dir); strings.Contains(d, "retry with jitter") {
		t.Errorf("denied repo memory reached session start:\n%s", d)
	}
	if out, _ := captureRun(t, "recall", "retry jitter webhook"); strings.Contains(out, "[rejected] retry with jitter") {
		t.Errorf("denied repo memory reached recall:\n%s", out)
	}
	if out, _ := captureRun(t, "repo"); !strings.Contains(out, "keeps repo memory out of mcp, auto") {
		t.Errorf("repo list does not name the policy:\n%s", out)
	}
	_ = os.Remove(pol)

	// Both add an entry at once; the merge takes both.
	if _, err = captureRun(t, "repo", "add", "--kind", "gotcha", "the staging receiver drops payloads over 1 MB"); err != nil {
		t.Fatal(err)
	}
	repoGit(t, bob, "commit", "-qam", "bob entry")
	repoPerson(t, base, "alice")
	t.Chdir(alice)
	if _, err = captureRun(t, "repo", "add", "--kind", "decision", "signatures use the delivery id, not the attempt"); err != nil {
		t.Fatal(err)
	}
	repoGit(t, alice, "commit", "-qam", "alice entry")
	repoGit(t, alice, "push", "-q", "origin", "main")
	t.Chdir(bob)
	repoGit(t, bob, "pull", "-q", "--no-edit", "origin", "main")
	entries, bad, err := sources.ReadRepoMemory(sources.RepoMemoryPath(bob))
	if err != nil || bad != 0 || len(entries) != 4 {
		b, _ := os.ReadFile(sources.RepoMemoryPath(bob))
		t.Fatalf("after merge: %d entries, %d bad, %v\n%s", len(entries), bad, err, b)
	}
}

// A reversal an agent reported in a session is proposed as a rejected entry,
// and its provenance names the agent and the day only.
func TestRepoCandidatesFromASession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture names a Claude project folder the unix way")
	}
	base := repoStandBase(t)
	home := repoPerson(t, base, "erin")
	root := filepath.Join(base, "erin", "webhooks")
	repoGit(t, base, "init", "-q", root)
	folder := filepath.Join(home, "claude", strings.ReplaceAll(root, "/", "-"))
	lines := []string{
		`{"type":"user","sessionId":"s1","cwd":"` + root + `","timestamp":"2026-08-03T10:00:00Z","message":{"role":"user","content":"deliveries get duplicated, add retry with jitter"}}`,
		`{"type":"assistant","sessionId":"s1","cwd":"` + root + `","timestamp":"2026-08-03T10:35:00Z","message":{"role":"assistant","content":[{"type":"text","text":"Reverted the retry with jitter: it broke idempotency because each attempt got a new delivery id.\nWe decided to keep one delivery id per event and let the outbox own retries."}]}}`,
	}
	writeClaudeFixture(t, filepath.Join(folder, "s1.jsonl"), "s1", lines)
	t.Chdir(root)
	out, err := captureRun(t, "repo", "candidates")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "rejected Reverted the retry with jitter") || !strings.Contains(out, "claude 2026-08-03") {
		t.Fatalf("candidates:\n%s", out)
	}
	if !strings.Contains(out, "decision") {
		t.Errorf("the session's decision is not proposed:\n%s", out)
	}
	if _, err := os.Stat(sources.RepoMemoryPath(root)); err == nil {
		t.Error("candidates wrote the file")
	}
	if out, err := captureRun(t, "repo", "candidates", "--since", "1d"); err != nil || !strings.Contains(out, "no candidates") {
		t.Errorf("--since kept old candidates: %q %v", out, err)
	}
}

func TestRepoAddRefusesWithoutRedaction(t *testing.T) {
	t.Setenv("DEJA_NO_REDACT", "1")
	if _, _, err := repoSafeText("anything"); err == nil || !strings.Contains(err.Error(), "DEJA_NO_REDACT") {
		t.Fatalf("err = %v", err)
	}
}

func TestRepoSafeTextMasksSecretsAndHomes(t *testing.T) {
	out, masked, err := repoSafeText("use token ghp_abcdefghijklmnopqrstuvwxyz0123456789 from /Users/alice/src/app/.env and mail ops@corp.example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"ghp_", "/Users/alice", "ops@"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q survived: %s", leak, out)
		}
	}
	if masked < 3 {
		t.Errorf("masked = %d in %q", masked, out)
	}
	if _, _, err := repoSafeText(strings.Repeat("word ", sources.RepoTextMax)); err == nil {
		t.Error("an entry over the cap was accepted")
	}
	if _, _, err := repoSafeText(" \n "); err == nil {
		t.Error("an empty entry was accepted")
	}
}

func TestRepoRememberThroughMCP(t *testing.T) {
	base := repoStandBase(t)
	repoPerson(t, base, "carol")
	root := filepath.Join(base, "carol", "api")
	repoGit(t, base, "init", "-q", root)
	t.Chdir(root)
	dir := os.Getenv("DEJA_INDEX_DIR")
	out, err := callMCPTool(dir, "remember", []byte(`{"text":"the vendored client is patched, do not bump it","project":"repo:gotcha"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "not committed") {
		t.Errorf("reply = %q", out)
	}
	entries, _, err := sources.ReadRepoMemory(sources.RepoMemoryPath(root))
	if err != nil || len(entries) != 1 || entries[0].Kind != "gotcha" {
		t.Fatalf("entries %v err %v", entries, err)
	}
	if _, err := callMCPTool(dir, "remember", []byte(`{"text":"x","project":"repo:opinion"}`)); err == nil {
		t.Error("an unknown kind was accepted")
	}
	// A plain project still files a note on this machine.
	if _, err := callMCPTool(dir, "remember", []byte(`{"text":"local only","project":"api"}`)); err != nil {
		t.Fatal(err)
	}
	if entries, _, _ := sources.ReadRepoMemory(sources.RepoMemoryPath(root)); len(entries) != 1 {
		t.Errorf("a note went into the repository: %v", entries)
	}
}

func TestRepoCommandErrors(t *testing.T) {
	base := repoStandBase(t)
	repoPerson(t, base, "dan")
	t.Chdir(base)
	if _, err := captureRun(t, "repo"); err == nil || !strings.Contains(err.Error(), "not inside a git repository") {
		t.Errorf("outside a repo: %v", err)
	}
	root := filepath.Join(base, "r")
	repoGit(t, base, "init", "-q", root)
	t.Chdir(root)
	if out, err := captureRun(t, "repo"); err != nil || !strings.Contains(out, "no .deja/memory.jsonl") {
		t.Errorf("empty list: %q %v", out, err)
	}
	if _, err := captureRun(t, "repo", "add", "ffffffffff"); err == nil || !strings.Contains(err.Error(), "not a candidate id") {
		t.Errorf("unknown id: %v", err)
	}
	if _, err := captureRun(t, "repo", "add", "--kind", "opinion", "x"); err == nil {
		t.Error("unknown kind accepted")
	}
	if _, err := captureRun(t, "repo", "add"); err == nil {
		t.Error("add with nothing accepted")
	}
	if _, err := captureRun(t, "repo", "--bogus"); err == nil {
		t.Error("unknown flag accepted")
	}
	if _, err := captureRun(t, "repo", "candidates", "--limit", "0"); err == nil {
		t.Error("--limit 0 accepted")
	}
	if out, err := captureRun(t, "repo", "candidates"); err != nil || !strings.Contains(out, "no candidates") {
		t.Errorf("no candidates: %q %v", out, err)
	}
	if _, err := captureRun(t, "repo", "add", "--kind=decision", "--", "-v is the verbose flag"); err != nil {
		t.Fatal(err)
	}
	if out, err := captureRun(t, "repo", "add", "--kind", "decision", "-v is the verbose flag"); err == nil {
		t.Errorf("a dash word without -- was taken: %q", out)
	}
	out, err := captureRun(t, "repo", "--json")
	if err != nil || !strings.Contains(out, `"kind":"decision"`) {
		t.Errorf("--json: %q %v", out, err)
	}
	if err := os.WriteFile(sources.RepoMemoryPath(root), []byte("<<<<<<< HEAD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, _ := captureRun(t, "repo"); !strings.Contains(out, "cannot read") {
		t.Errorf("bad line not counted: %q", out)
	}
	if _, err := captureRun(t, "repo", "add", "--kind", "decision", "another"); err == nil || !strings.Contains(err.Error(), "fix or delete") {
		t.Errorf("write over a conflict: %v", err)
	}
}
