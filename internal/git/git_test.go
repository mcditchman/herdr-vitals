package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repo builds a repository out of files rather than by running git, so the
// tests describe the format they parse and need no git install.
type repo struct {
	root   string
	gitDir string
}

func newRepo(t *testing.T) repo {
	t.Helper()

	root := t.TempDir()
	r := repo{root: root, gitDir: filepath.Join(root, ".git")}
	r.write(t, filepath.Join(r.gitDir, "HEAD"), "ref: refs/heads/main\n")

	return r
}

func (r repo) write(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r repo) head(t *testing.T, content string) repo {
	t.Helper()
	r.write(t, filepath.Join(r.gitDir, "HEAD"), content)

	return r
}

func (r repo) subdir(t *testing.T, path string) string {
	t.Helper()

	dir := filepath.Join(r.root, path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	return dir
}

func mustBranch(t *testing.T, dir, want string) {
	t.Helper()

	if got := Branch(dir); got != want {
		t.Fatalf("Branch(%q) = %q, want %q", dir, got, want)
	}
}

func TestTheCheckedOutBranchIsRead(t *testing.T) {
	r := newRepo(t).head(t, "ref: refs/heads/feature/login\n")
	mustBranch(t, r.root, "feature/login")
}

func TestTheRepositoryIsFoundFromASubdirectory(t *testing.T) {
	r := newRepo(t).head(t, "ref: refs/heads/side\n")
	mustBranch(t, r.subdir(t, "internal/app"), "side")
}

func TestATrailingSeparatorIsTolerated(t *testing.T) {
	// Herdr reports Windows pane directories with a trailing backslash.
	r := newRepo(t).head(t, "ref: refs/heads/side\n")
	mustBranch(t, r.root+string(filepath.Separator), "side")
}

func TestADirectoryOutsideARepositoryIsNotOne(t *testing.T) {
	mustBranch(t, t.TempDir(), "")
}

func TestOnlyAnAbsolutePathIsRead(t *testing.T) {
	// A relative path would resolve against the plugin's own directory.
	mustBranch(t, "relative/path", "")
	mustBranch(t, "", "")
}

func TestADetachedHeadIsAbbreviated(t *testing.T) {
	r := newRepo(t).head(t, "aaf1fd85f68047764760489dbfc3ecb5ab9d0cb8\n")
	mustBranch(t, r.root, "aaf1fd8")
}

func TestASha256HeadIsAbbreviatedToo(t *testing.T) {
	r := newRepo(t).head(t, "9c8f2b1a0123456789abcdef0123456789abcdef0123456789abcdef08b1cd2e\n")
	mustBranch(t, r.root, "9c8f2b1")
}

func TestAHeadHoldingSomethingElseIsNothing(t *testing.T) {
	// A hash of the wrong length is not one: that is what bounds the read.
	for _, content := range []string{"", "\n", "ref: refs/tags/v1.0.0\n", "not a hash\n",
		"aaf1fd85f68047764760489dbfc3ecb5ab9d0c\n"} {
		r := newRepo(t).head(t, content)
		if got := Branch(r.root); got != "" {
			t.Errorf("HEAD %q gave %q, want nothing", content, got)
		}
	}
}

func TestARebaseNamesTheBranchItSetAside(t *testing.T) {
	for _, state := range []string{"rebase-merge", "rebase-apply"} {
		r := newRepo(t).head(t, "e87a64205f30ad32f455ff99a335cb89669f3321\n")
		r.write(t, filepath.Join(r.gitDir, state, "head-name"), "refs/heads/topic\n")

		if got := Branch(r.root); got != "topic" {
			t.Errorf("%s: branch %q, want %q", state, got, "topic")
		}
	}
}

func TestAWorktreeIsFollowedToItsOwnHead(t *testing.T) {
	main := newRepo(t)
	worktreeGitDir := filepath.Join(main.gitDir, "worktrees", "wt")
	main.write(t, filepath.Join(worktreeGitDir, "HEAD"), "ref: refs/heads/feature-login\n")

	tree := t.TempDir()
	main.write(t, filepath.Join(tree, ".git"), "gitdir: "+worktreeGitDir+"\n")

	mustBranch(t, tree, "feature-login")
}

func TestARelativeGitdirIsResolvedAgainstTheFileHoldingIt(t *testing.T) {
	// A submodule records its git directory relative to its own working tree.
	parent := newRepo(t)
	moduleGitDir := filepath.Join(parent.gitDir, "modules", "vendor")
	parent.write(t, filepath.Join(moduleGitDir, "HEAD"), "ref: refs/heads/pinned\n")

	module := parent.subdir(t, "vendor")
	parent.write(t, filepath.Join(module, ".git"), "gitdir: ../.git/modules/vendor\n")

	mustBranch(t, module, "pinned")
}

func TestAnUnreadableGitFileIsNoRepository(t *testing.T) {
	root := t.TempDir()
	r := repo{root: root, gitDir: filepath.Join(root, ".git")}
	r.write(t, filepath.Join(root, ".git"), "not a gitdir line\n")

	mustBranch(t, root, "")
}

func TestOnlyTheFirstLineOfARefIsRead(t *testing.T) {
	r := newRepo(t).head(t, "ref: refs/heads/side\nrubbish\n")
	mustBranch(t, r.root, "side")
}

func TestAHugeHeadIsNotReadWhole(t *testing.T) {
	r := newRepo(t).head(t, strings.Repeat("a", 2*maxRefFileSize))

	// The bounded read leaves a truncated hash, which is not a commit.
	mustBranch(t, r.root, "")
}
