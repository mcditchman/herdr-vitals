// Package git reads which branch a directory's repository has checked out from
// the files under .git, never by running git: it runs every poll for every
// agent pane, and a process spawn per pane would cost more than the poll.
//
// Adapted from kryptamine/herdr-auto-title (MIT).
package git

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// shortCommitLength is how much of a detached HEAD's hash names it, matching
// git's own abbreviation.
const shortCommitLength = 7

// Whole hash lengths: sha1, and sha256 for repositories built with it. The
// length check keeps a HEAD holding anything else from reading as a commit.
const (
	sha1Length   = 40
	sha256Length = 64
)

// maxRefFileSize bounds what is read from a ref file. These hold one short
// line, and the paths reaching here derive from a pane's directory.
const maxRefFileSize = 4 << 10

// branchPrefix is what HEAD carries when a branch is checked out.
const branchPrefix = "ref: refs/heads/"

// Branch reports what the repository holding dir has checked out: the branch
// name, the branch a rebase in progress set aside, or a detached HEAD's short
// hash. It is "" outside a repository or when .git cannot be read.
func Branch(dir string) string {
	gitDir, found := discover(dir)
	if !found {
		return ""
	}

	head, ok := readRef(filepath.Join(gitDir, "HEAD"))
	if !ok {
		return ""
	}

	if branch, ok := strings.CutPrefix(head, branchPrefix); ok {
		return branch
	}

	// A rebase detaches HEAD and records the branch it came from, which is
	// still where the work is happening.
	if branch, rebasing := rebasingBranch(gitDir); rebasing {
		return branch
	}

	return abbreviate(head)
}

// discover walks up from dir to the repository holding it and returns the
// directory holding HEAD. In a worktree or submodule .git is a file pointing
// elsewhere.
func discover(dir string) (string, bool) {
	if dir == "" || !filepath.IsAbs(dir) {
		return "", false
	}

	for dir = filepath.Clean(dir); ; {
		candidate := filepath.Join(dir, ".git")

		info, err := os.Stat(candidate)
		switch {
		case err != nil:
		case info.IsDir():
			return candidate, true
		default:
			return linkedGitDir(candidate)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}

		dir = parent
	}
}

// linkedGitDir follows the `gitdir:` line a worktree or submodule leaves in
// place of a .git directory. A relative path is relative to the file's
// directory.
func linkedGitDir(gitFile string) (string, bool) {
	line, ok := readRef(gitFile)
	if !ok {
		return "", false
	}

	target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if target == line || target == "" {
		return "", false
	}

	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(gitFile), target)
	}

	return filepath.Clean(target), true
}

// rebasingBranch reports the branch a rebase in progress set aside. The merge
// backend (default) and the apply backend (`--apply`, `git am`) both record it.
func rebasingBranch(gitDir string) (string, bool) {
	for _, state := range []string{"rebase-merge", "rebase-apply"} {
		line, ok := readRef(filepath.Join(gitDir, state, "head-name"))
		if !ok {
			continue
		}

		if branch, ok := strings.CutPrefix(line, "refs/heads/"); ok && branch != "" {
			return branch, true
		}
	}

	return "", false
}

// readRef returns the first line of a ref file, or false when there is nothing
// to read. The read is bounded: these files hold one line.
func readRef(path string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()

	content, err := io.ReadAll(io.LimitReader(file, maxRefFileSize))
	if err != nil {
		return "", false
	}

	line, _, _ := strings.Cut(string(content), "\n")

	line = strings.TrimSpace(line)

	return line, line != ""
}

// abbreviate shortens a detached HEAD's hash to the length git prints. A HEAD
// holding anything else yields "", so a file that is not a ref never reaches
// the sidebar.
func abbreviate(head string) string {
	whole := len(head) == sha1Length || len(head) == sha256Length
	if !whole || !isHex(head) {
		return ""
	}

	return head[:shortCommitLength]
}

func isHex(value string) bool {
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}

	return true
}
