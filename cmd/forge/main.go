package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/forgehubproject/forge/internal/credential"
	"github.com/forgehubproject/forge/internal/fhr"
	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/forgehubproject/forge/internal/gitrepo"
	"github.com/forgehubproject/forge/internal/handler"
	"github.com/forgehubproject/forge/internal/mcpserver"
	"github.com/forgehubproject/forge/internal/webdiff"
	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gogithttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// version is the release version, injected at build time via
// -ldflags "-X main.version=v1.0.0". Defaults to "dev" for local builds.
var version = "dev"

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// defaultRegistry builds the handler registry for the repository the command
// was run in. A command's handler subprocesses are bound to no context narrower
// than the process: they live as long as the command does, and a terminal's
// interrupt reaches them through the process group.
func defaultRegistry() *handler.Registry {
	return forgerepo.Registry(context.Background(), forgerepo.FindRoot())
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "forge",
		Short:   "Git, but for everything.",
		Long:    "Forge is a format-aware version control system with semantic diff and merge for any file type.",
		Version: version,
	}
	root.SetVersionTemplate("forge {{.Version}}\n")
	root.AddCommand(
		initCmd(),
		loginCmd(),
		cloneCmd(),
		statusCmd(),
		diffCmd(),
		showCmd(),
		mergeCmd(),
		mergeFileCmd(),
		mergeToolCmd(),
		logCmd(),
		pushCmd(),
		pullCmd(),
		sourceCmd(),
		formatsCmd(),
		mcpCmd(),
		gitPassthrough("add", "Stage file contents (delegates to git)"),
		gitPassthrough("commit", "Record staged changes (delegates to git)"),
		gitPassthrough("branch", "List, create or delete branches (delegates to git)"),
		gitPassthrough("checkout", "Switch branches or restore files (delegates to git)"),
		gitPassthrough("switch", "Switch branches (delegates to git)"),
		gitPassthrough("fetch", "Download objects and refs from remote (delegates to git)"),
		gitPassthrough("stash", "Stash working tree changes (delegates to git)"),
		gitPassthrough("reset", "Reset HEAD or working tree (delegates to git)"),
		gitPassthrough("restore", "Restore working tree files (delegates to git)"),
		gitPassthrough("rebase", "Reapply commits on top of another branch (delegates to git)"),
		gitPassthrough("tag", "Create, list or delete tags (delegates to git)"),
		gitPassthrough("remote", "Manage remote connections (delegates to git)"),
		// Identity and git options live in git's config — the single source of
		// truth forge reads through (issue #30). No forge-side config store.
		gitPassthrough("config", "Get and set repository or global options (delegates to git)"),
	)
	return root
}

// ── forge init ──────────────────────────────────────────────────────────────────────────────────────

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init [directory]",
		Short: "Create a new Forge repository",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runInit,
	}
}

func runInit(_ *cobra.Command, args []string) error {
	dir := "."
	if len(args) == 1 {
		dir = args[0]
	}

	if _, err := gogit.PlainInit(dir, false); err != nil && !errors.Is(err, gogit.ErrRepositoryAlreadyExists) {
		return fmt.Errorf("init failed: %w", err)
	}

	if err := setupGitMergeDriver(dir); err != nil {
		fmt.Fprintf(os.Stderr, "forge: warning: could not configure git merge driver: %v\n", err)
	}

	abs, _ := filepath.Abs(dir)
	fmt.Printf("Initialized Forge repository in %s\n", abs)
	return nil
}

func setupGitMergeDriver(repoDir string) error {
	attrPath := filepath.Join(repoDir, ".gitattributes")
	existing, _ := os.ReadFile(attrPath)
	forgeFormats := forgerepo.LoadForgeFormats(repoDir)
	var toAdd []string
	for ext := range forgeFormats {
		entry := "*" + ext + "  merge=forge"
		if !bytes.Contains(existing, []byte("*"+ext)) {
			toAdd = append(toAdd, entry)
		}
	}
	sort.Strings(toAdd)
	if len(toAdd) > 0 {
		f, err := os.OpenFile(attrPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
			fmt.Fprintln(f)
		}
		fmt.Fprintln(f, "# Forge semantic merge drivers")
		for _, e := range toAdd {
			fmt.Fprintln(f, e)
		}
		f.Close()
	}

	gitConfigPath := filepath.Join(repoDir, ".git", "config")
	gitConfig, _ := os.ReadFile(gitConfigPath)
	if !bytes.Contains(gitConfig, []byte(`[merge "forge"]`)) {
		f, err := os.OpenFile(gitConfigPath, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		fmt.Fprintf(f, "\n[merge \"forge\"]\n\tname = Forge semantic merge\n\tdriver = forge merge-file %%O %%A %%B %%P\n")
		f.Close()
	}

	return nil
}

// ── forge status ────────────────────────────────────────────────────────────────────────────────────

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show working tree status with handler annotations",
		RunE:  runStatus,
	}
}

func runStatus(_ *cobra.Command, _ []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	r, err := gogit.PlainOpenWithOptions(cwd, &gogit.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return fmt.Errorf("not a git repository")
	}

	if head, err := r.Head(); err == nil {
		if head.Name().IsBranch() {
			fmt.Printf("On branch \x1b[1m%s\x1b[0m\n", head.Name().Short())
			printAheadBehind()
		} else {
			fmt.Printf("HEAD detached at %s\n", head.Hash().String()[:7])
		}
	}

	if gitDir, err := exec.Command("git", "rev-parse", "--git-dir").Output(); err == nil {
		mergeHead := filepath.Join(strings.TrimSpace(string(gitDir)), "MERGE_HEAD")
		if _, mergeErr := os.Stat(mergeHead); mergeErr == nil {
			printMergeStatus()
		}
	}

	// Surface .forge/formats drift (issue #34): a listed format with no
	// installed handler is silently inactive — flag it with the repair command.
	if missing := missingHandlerExts(cwd); len(missing) > 0 {
		fmt.Printf("\x1b[33mwarning:\x1b[0m formats listed in .forge/formats with no installed handler: %s\n",
			strings.Join(missing, ", "))
		fmt.Println("         install them with: forge formats install")
	}

	wt, err := r.Worktree()
	if err != nil {
		return err
	}

	st, err := wt.Status()
	if err != nil {
		return err
	}

	if st.IsClean() {
		fmt.Println("nothing to commit, working tree clean")
		return nil
	}

	reg := defaultRegistry()

	paths := make([]string, 0, len(st))
	for p := range st {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var stagedPaths, unstagedPaths []string
	for _, p := range paths {
		fs := st[p]
		s, w := rune(fs.Staging), rune(fs.Worktree)
		if s == '?' && w == '?' {
			// Untracked files come from git below — go-git's Status() has only
			// partial .gitignore support and doesn't collapse untracked dirs.
			continue
		}
		if s != ' ' && s != '?' {
			stagedPaths = append(stagedPaths, p)
		}
		if w != ' ' && w != '?' {
			unstagedPaths = append(unstagedPaths, p)
		}
	}

	// Untracked list defers to git for the full ignore stack (.gitignore, nested
	// .gitignores, .git/info/exclude, core.excludesFile) and git's directory
	// collapsing — the source of truth forge wraps. Path-level ignoring is
	// .gitignore's job; forge's own ignoring is format-level (forge formats ignore).
	untrackedPaths := gitUntrackedFiles(cwd)

	// go-git may report "not clean" purely because of ignored files it surfaces
	// as untracked; if git disagrees and there are no real changes, we're clean.
	if len(stagedPaths) == 0 && len(unstagedPaths) == 0 && len(untrackedPaths) == 0 {
		fmt.Println("nothing to commit, working tree clean")
		return nil
	}

	statusWord := func(code rune) string {
		switch code {
		case 'A':
			return "new file:  "
		case 'D':
			return "deleted:   "
		case 'R':
			return "renamed:   "
		case 'C':
			return "copied:    "
		default:
			return "modified:  "
		}
	}

	printStagedEntry := func(p string) {
		label := handlerLabel(p, reg)
		word := statusWord(rune(st[p].Staging))
		fmt.Printf("\x1b[32m\t%s%-38s\x1b[0m %s\n", word, p, label)
	}

	printUnstagedEntry := func(p string) {
		label := handlerLabel(p, reg)
		word := statusWord(rune(st[p].Worktree))
		fmt.Printf("\x1b[31m\t%s%-38s\x1b[0m %s\n", word, p, label)
	}

	printUntrackedEntry := func(p string) {
		// git collapses a wholly-untracked directory to "dir/" — no per-file
		// handler applies, so print it bare, matching git.
		if strings.HasSuffix(p, "/") {
			fmt.Printf("\x1b[31m\t%-49s\x1b[0m\n", p)
			return
		}
		label := handlerLabel(p, reg)
		fmt.Printf("\x1b[31m\t%-49s\x1b[0m %s\n", p, label)
	}

	if len(stagedPaths) > 0 {
		fmt.Println("Changes to be committed:")
		fmt.Println("  \x1b[2m(use \"forge restore --staged <file>...\" to unstage)\x1b[0m")
		for _, p := range stagedPaths {
			printStagedEntry(p)
		}
		fmt.Println()
	}

	if len(unstagedPaths) > 0 {
		fmt.Println("Changes not staged for commit:")
		fmt.Println("  \x1b[2m(use \"forge add <file>...\" to update what will be committed)\x1b[0m")
		fmt.Println("  \x1b[2m(use \"forge restore <file>...\" to discard changes in working directory)\x1b[0m")
		for _, p := range unstagedPaths {
			printUnstagedEntry(p)
		}
		fmt.Println()
	}

	if len(untrackedPaths) > 0 {
		fmt.Println("Untracked files:")
		fmt.Println("  \x1b[2m(use \"forge add <file>...\" to include in what will be committed)\x1b[0m")
		for _, p := range untrackedPaths {
			printUntrackedEntry(p)
		}
		fmt.Println()
	}

	return nil
}

// gitUntrackedFiles returns the untracked paths git itself would show —
// respecting the full ignore stack (.gitignore, nested .gitignores,
// .git/info/exclude, core.excludesFile) and collapsing wholly-untracked
// directories to a single "dir/" entry, exactly like `git status`. forge wraps
// git and stores git objects, so it defers to git's ignore semantics rather
// than go-git's partial Status() implementation.
func gitUntrackedFiles(dir string) []string {
	root := dir
	if top, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		root = strings.TrimSpace(string(top))
	}
	cmd := exec.Command("git", "ls-files", "--others", "--exclude-standard", "--directory", "--no-empty-directory")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			paths = append(paths, line)
		}
	}
	sort.Strings(paths)
	return paths
}

func handlerLabel(path string, reg *handler.Registry) string {
	h, err := reg.Resolve(path)
	if err != nil {
		return "\x1b[31m[no handler]\x1b[0m"
	}
	name := "text"
	if n, ok := h.(handler.Namer); ok {
		name = n.Format()
	}
	return fmt.Sprintf("\x1b[36m[%s]\x1b[0m", name)
}

func printAheadBehind() {
	upstreamOut, err := exec.Command("git", "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}").Output()
	if err != nil {
		return
	}
	upstream := strings.TrimSpace(string(upstreamOut))

	aheadOut, _ := exec.Command("git", "rev-list", "--count", upstream+"..HEAD").Output()
	behindOut, _ := exec.Command("git", "rev-list", "--count", "HEAD.."+upstream).Output()
	ahead, _ := strconv.Atoi(strings.TrimSpace(string(aheadOut)))
	behind, _ := strconv.Atoi(strings.TrimSpace(string(behindOut)))

	switch {
	case ahead > 0 && behind > 0:
		fmt.Printf("Your branch and '%s' have diverged,\nand have %d and %d different commits each, respectively.\n", upstream, ahead, behind)
		fmt.Println("  (use \"forge pull\" to update your local branch)")
	case ahead > 0:
		noun := "commit"
		if ahead != 1 {
			noun = "commits"
		}
		fmt.Printf("Your branch is ahead of '%s' by %d %s.\n", upstream, ahead, noun)
		fmt.Println("  (use \"forge push\" to publish your local commits)")
	case behind > 0:
		noun := "commit"
		if behind != 1 {
			noun = "commits"
		}
		fmt.Printf("Your branch is behind '%s' by %d %s, and can be fast-forwarded.\n", upstream, behind, noun)
		fmt.Println("  (use \"forge pull\" to update your local branch)")
	}
}

func printMergeStatus() {
	out, _ := exec.Command("git", "status", "--porcelain").Output()

	type unmergedEntry struct{ code, path string }
	var entries []unmergedEntry
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		xy := line[:2]
		path := strings.TrimSpace(line[3:])
		if strings.ContainsAny(xy, "U") || xy == "AA" || xy == "DD" {
			entries = append(entries, unmergedEntry{xy, path})
		}
	}

	conflictLabel := func(xy string) string {
		switch xy {
		case "AA":
			return "both added:     "
		case "UU":
			return "both modified:  "
		case "DD":
			return "both deleted:   "
		case "AU", "UA":
			return "added/modified: "
		case "DU", "UD":
			return "deleted/modified:"
		default:
			return "unmerged:       "
		}
	}

	fmt.Println("You have unmerged paths.")
	fmt.Println("  (fix conflicts and run \"forge commit\")")
	fmt.Println("  (use \"forge merge --abort\" to abort the merge)")
	if len(entries) > 0 {
		fmt.Println()
		fmt.Println("Unmerged paths:")
		fmt.Println("  (use \"forge mergetool\" to resolve · \"forge add <file>\" to mark resolved)")
		reg := defaultRegistry()
		for _, e := range entries {
			label := handlerLabel(e.path, reg)
			fmt.Printf("\x1b[31m\t%s%-38s\x1b[0m %s\n", conflictLabel(e.code), e.path, label)
		}
	}
	fmt.Println()
}

// ── forge clone ───────────────────────────────────────────────────────────────────────────────────

func cloneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clone <url> [directory]",
		Short: "Clone a Forge repository and report required handlers",
		Args:  cobra.RangeArgs(1, 2),
		RunE:  runClone,
	}
	cmd.Flags().String("token", "", "Personal access token for HTTPS authentication")
	cmd.Flags().String("ssh-key", defaultSSHKey(), "Path to SSH private key")
	cmd.Flags().String("ssh-password", "", "Passphrase for the SSH private key (if encrypted)")
	return cmd
}

func runClone(cmd *cobra.Command, args []string) error {
	rawURL := args[0]
	dir := ""
	if len(args) == 2 {
		dir = args[1]
	} else {
		dir = repoNameFromURL(rawURL)
	}

	token, _ := cmd.Flags().GetString("token")
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	sshPassword, _ := cmd.Flags().GetString("ssh-password")

	cloneOpts, err := buildCloneOptions(rawURL, token, sshKey, sshPassword)
	if err != nil {
		return err
	}
	cloneOpts.Progress = os.Stdout

	fmt.Printf("Cloning into '%s'...\n", dir)

	_, err = gogit.PlainClone(dir, false, cloneOpts)
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		// Match git: cloning a freshly-created empty repo is a normal first
		// step, not a failure. Leave an initialized local repo with `origin`
		// configured so the usual add/commit/push flow works.
		fmt.Println("warning: You appear to have cloned an empty repository.")
		if initErr := initEmptyClone(dir, rawURL); initErr != nil {
			return fmt.Errorf("clone failed: %w", initErr)
		}
	} else if err != nil {
		return fmt.Errorf("clone failed: %w", err)
	}

	if err := setupGitMergeDriver(dir); err != nil {
		fmt.Fprintf(os.Stderr, "forge: warning: could not configure git merge driver: %v\n", err)
	}

	reportMissingHandlers(dir)
	return nil
}

// initEmptyClone recovers from go-git's ErrEmptyRemoteRepository: depending on
// the version, PlainClone may or may not have left a usable repo behind, so
// open-or-init the directory and make sure the `origin` remote is registered.
func initEmptyClone(dir, rawURL string) error {
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		if repo, err = gogit.PlainInit(dir, false); err != nil {
			return err
		}
	}
	if _, err := repo.Remote("origin"); err == nil {
		return nil // PlainClone already registered it
	}
	_, err = repo.CreateRemote(&gogitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{rawURL},
	})
	return err
}

func buildCloneOptions(rawURL, token, sshKeyPath, sshPassword string) (*gogit.CloneOptions, error) {
	opts := &gogit.CloneOptions{URL: rawURL}

	if isSSHURL(rawURL) {
		if agent, err := gogitssh.NewSSHAgentAuth("git"); err == nil {
			opts.Auth = agent
			return opts, nil
		}
		keys, err := gogitssh.NewPublicKeysFromFile("git", sshKeyPath, sshPassword)
		if err != nil {
			return nil, fmt.Errorf(
				"SSH agent unavailable and could not load key %s: %w\n"+
					"  Start an SSH agent and run: ssh-add %s\n"+
					"  Or pass the key passphrase: forge clone <url> --ssh-password <passphrase>\n"+
					"  Or clone over HTTPS with a token: forge clone <https-url> --token <token>",
				sshKeyPath, err, sshKeyPath,
			)
		}
		opts.Auth = keys
		return opts, nil
	}

	if token == "" {
		for _, env := range []string{"FORGE_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
			if t := os.Getenv(env); t != "" {
				token = t
				break
			}
		}
	}
	if token != "" {
		opts.Auth = &gogithttp.BasicAuth{Username: "x-token", Password: token}
		return opts, nil
	}

	// Fall back to whatever git credential helper is already configured on
	// this machine (osxkeychain, wincred, libsecret, cache, store, ...) —
	// e.g. the credential `forge login` stores — before giving up.
	if username, password, ok := credential.Fill(rawURL); ok {
		opts.Auth = &gogithttp.BasicAuth{Username: username, Password: password}
	}

	return opts, nil
}

// ── forge login ───────────────────────────────────────────────────────────────────────────────────

func loginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login <url>",
		Short: "Authenticate against a ForgeHub server and store the credential for git/forge to reuse",
		Long: "Logs in to a ForgeHub server, mints a Personal Access Token, and stores it via git's\n" +
			"own credential-helper protocol — so plain `git` and `forge` both pick it up automatically\n" +
			"afterward, with no need to pass --token or paste it into environment variables.",
		Args: cobra.ExactArgs(1),
		RunE: runLogin,
	}
	cmd.Flags().String("email", "", "Account email (prompted if omitted)")
	cmd.Flags().String("password", "", "Account password (prompted if omitted; avoid passing on shared machines)")
	return cmd
}

func runLogin(cmd *cobra.Command, args []string) error {
	baseURL := strings.TrimRight(args[0], "/")
	if baseURL == "" {
		return errors.New("url is required")
	}

	email, _ := cmd.Flags().GetString("email")
	password, _ := cmd.Flags().GetString("password")

	if email == "" {
		fmt.Print("Email: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading email: %w", err)
		}
		email = strings.TrimSpace(line)
	}
	if email == "" {
		return errors.New("email is required")
	}

	if password == "" {
		fmt.Print("Password: ")
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return fmt.Errorf("reading password: %w", err)
		}
		password = string(pw)
	}
	if password == "" {
		return errors.New("password is required")
	}

	sessionToken, err := forgeHubLogin(baseURL, email, password)
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	hostname, _ := os.Hostname()
	tokenName := "forge-cli@" + hostname
	pat, err := forgeHubCreateToken(baseURL, sessionToken, tokenName)
	if err != nil {
		return fmt.Errorf("logged in, but could not create a personal access token: %w", err)
	}

	if err := credential.Approve(baseURL, email, pat); err != nil {
		fmt.Fprintf(os.Stderr, "forge: warning: could not store credential via git credential helper: %v\n", err)
		fmt.Printf("Your token (save it yourself, it will not be shown again): %s\n", pat)
		return nil
	}

	fmt.Printf("Logged in as %s.\n", email)
	fmt.Printf("Credential stored via git's credential helper — git and forge will use it automatically for %s.\n", baseURL)

	if info, err := fetchServerInfo(baseURL); err == nil && info.SSHEnabled {
		host := info.SSHHost
		if host == "" {
			if u, err := url.Parse(baseURL); err == nil {
				host = u.Hostname()
			}
		}
		port := 22
		if info.SSHPort != 0 {
			port = info.SSHPort
		}
		fmt.Printf("\nSSH is enabled on this server (port %d).\n", port)
		if info.SSHFingerprint != "" {
			fmt.Printf("Host key fingerprint: %s\n", info.SSHFingerprint)
			fmt.Printf("\nBefore trusting this server's host key, verify the fingerprint above matches:\n")
			fmt.Printf("  ssh-keyscan -p %d %s | ssh-keygen -lf -\n", port, host)
			fmt.Printf("If it matches, add the key to your known hosts:\n")
			fmt.Printf("  ssh-keyscan -p %d %s >> ~/.ssh/known_hosts\n", port, host)
		} else {
			fmt.Printf("To trust this server's host key, run:\n")
			fmt.Printf("  ssh-keyscan -p %d %s >> ~/.ssh/known_hosts\n", port, host)
		}
	}

	return nil
}

type serverInfo struct {
	SSHEnabled     bool   `json:"sshEnabled"`
	SSHPort        int    `json:"sshPort"`
	SSHHost        string `json:"sshHost"`
	SSHFingerprint string `json:"sshFingerprint"`
}

func fetchServerInfo(baseURL string) (*serverInfo, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(baseURL + "/server/info")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var info serverInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func forgeHubLogin(baseURL, email, password string) (string, error) {
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		return "", err
	}
	resp, err := http.Post(baseURL+"/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(forgeHubErrorMessage(data, resp.StatusCode))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("server did not return a session token")
	}
	return out.Token, nil
}

func forgeHubCreateToken(baseURL, sessionToken, name string) (string, error) {
	body, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/auth/tokens", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sessionToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusCreated {
		return "", errors.New(forgeHubErrorMessage(data, resp.StatusCode))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("server did not return a token")
	}
	return out.Token, nil
}

func forgeHubErrorMessage(body []byte, status int) string {
	var out struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &out) == nil && out.Error != "" {
		return out.Error
	}
	return fmt.Sprintf("unexpected status %d", status)
}

func isSSHURL(rawURL string) bool {
	if strings.HasPrefix(rawURL, "git@") {
		return true
	}
	u, err := url.Parse(rawURL)
	return err == nil && u.Scheme == "ssh"
}

func defaultSSHKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
		p := filepath.Join(home, ".ssh", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(home, ".ssh", "id_ed25519")
}

// missingHandlerExts returns the extensions listed in .forge/formats that no
// installed handler covers — the "half-configured repo" drift that clone and
// status both warn about (issue #34).
func missingHandlerExts(repoDir string) []string {
	forgeFormats := forgerepo.LoadForgeFormats(repoDir)
	if len(forgeFormats) == 0 {
		return nil
	}

	covered := map[string]bool{}
	for _, meta := range fhr.LoadInstalledHandlers() {
		for _, ext := range meta.Formats {
			covered[strings.ToLower(ext)] = true
		}
	}

	var missing []string
	for ext := range forgeFormats {
		if !covered[ext] {
			missing = append(missing, ext)
		}
	}
	sort.Strings(missing)
	return missing
}

func reportMissingHandlers(repoDir string) {
	missing := missingHandlerExts(repoDir)
	if len(missing) == 0 {
		return
	}

	fmt.Println()
	fmt.Println("This repository requires format handlers that are not installed:")
	for _, ext := range missing {
		fmt.Printf("  %s\n", ext)
	}
	fmt.Println("Install them all with: forge formats install")
	fmt.Println()
}

func repoNameFromURL(url string) string {
	url = strings.TrimRight(url, "/")
	url = strings.TrimSuffix(url, ".git")
	if i := strings.LastIndexAny(url, "/:'`"+`"`); i >= 0 {
		url = url[i+1:]
	}
	if url == "" {
		return "repo"
	}
	return url
}

// ── forge diff ───────────────────────────────────────────────────────────────────────────────────

func diffCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff [<revision> [<revision>]] [--] [<path>...]",
		Short: "Show semantic diff between the working tree and a revision, or between two revisions",
		Long: `Shows a semantic diff for every changed path a format handler claims, and
git's own text diff for the rest.

  forge diff                  the working tree against HEAD
  forge diff <revision>       the working tree against a revision
  forge diff <base> <head>    revision to revision

Paths may follow the revisions. Use "--" when an argument is both a revision
and a filename, and to name a path the working tree does not have.`,
		Args: cobra.ArbitraryArgs,
		RunE: runDiff,
	}
	cmd.Flags().Bool("web", false, "Render the diff in a local browser using the format's FHR renderer")
	cmd.Flags().Bool("no-open", false, "With --web, print the URL but do not launch a browser")
	return cmd
}

func runDiff(cmd *cobra.Command, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	repo, err := gitrepo.Open(cwd)
	if err != nil {
		return err
	}
	repoDir := forgerepo.FindRoot()

	reg := defaultRegistry()

	args, dashAt := forgerepo.ExpandRevRange(context.Background(), repoDir, args, cmd.ArgsLenAtDash())
	revs, rawPaths, err := forgerepo.SplitRevsAndPaths(context.Background(), repoDir, args, dashAt)
	if err != nil {
		return err
	}
	base, head, err := forgerepo.DiffSources(context.Background(), repoDir, revs)
	if err != nil {
		return err
	}
	paths, err := forgerepo.RelPaths(repoDir, rawPaths)
	if err != nil {
		return err
	}

	if web, _ := cmd.Flags().GetBool("web"); web {
		if len(paths) != 1 {
			return fmt.Errorf("forge diff --web needs exactly one file to render")
		}
		noOpen, _ := cmd.Flags().GetBool("no-open")
		return diffFileWeb(repoDir, reg, paths[0], base, head, !noOpen)
	}

	if len(paths) > 0 {
		files, err := forgerepo.ComparePaths(context.Background(), repoDir, base, head, paths)
		if err != nil {
			return err
		}
		return renderPaths(files, func(path string) error {
			return diffFile(repoDir, reg, path, base, head)
		})
	}

	var changed []string
	if len(revs) == 0 {
		// go-git's status, which also surfaces files the working tree has and
		// HEAD does not — untracked work a revision listing cannot report.
		changed, err = repo.ChangedFiles()
	} else {
		changed, err = forgerepo.ChangedPaths(context.Background(), repoDir, base, head, nil)
	}
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		fmt.Println("no changes")
		return nil
	}
	// A survey of everything that changed reports the paths it could not read and
	// still exits zero, as forge diff did before it took revisions; only a path
	// the caller named carries its failure into the exit status.
	_ = renderPaths(changed, func(path string) error {
		return diffFile(repoDir, reg, path, base, head)
	})
	return nil
}

func cleanPath(p string) string {
	return filepath.ToSlash(filepath.Clean(p))
}

// diffFile renders one path's change between two sources: the handler's change
// tree, or git's own text diff for a path no handler claims. An error is the
// caller's to report — one unreadable path does not end a multi-path diff.
func diffFile(repoDir string, reg *handler.Registry, path string, base, head forgerepo.Source) error {
	fc, err := forgerepo.CompareFile(context.Background(), repoDir, reg, path, base, head)
	if err != nil {
		return err
	}

	switch {
	case !fc.BaseFound && !fc.HeadFound:
		fmt.Printf("%s: not in %s or %s\n", path, base, head)
	case !fc.Semantic:
		c := exec.Command("git", forgerepo.GitDiffArgs(base, head, nil, []string{path})...)
		c.Dir = repoDir
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		_ = c.Run()
	default:
		renderDiff(path, fc.Diff)
	}
	return nil
}

// diffFileWeb computes the semantic diff locally and serves it to a loopback
// browser page rendered by the format's FHR renderer bundle (SPEC-RENDERING §3b).
func diffFileWeb(repoDir string, reg *handler.Registry, path string, base, head forgerepo.Source, openBrowser bool) error {
	fc, err := forgerepo.CompareFile(context.Background(), repoDir, reg, path, base, head)
	if err != nil {
		return err
	}
	if !fc.Semantic {
		return fmt.Errorf("--web needs a semantic handler; %s has none — use plain: forge diff %s", path, path)
	}
	if !fc.BaseFound && !fc.HeadFound {
		return fmt.Errorf("%s is not in %s or %s", path, base, head)
	}

	diffJSON, err := json.Marshal(fc.Diff)
	if err != nil {
		return fmt.Errorf("encoding diff: %w", err)
	}

	rendererPath, err := ensureRenderer(fc.HandlerID)
	if err != nil {
		return err
	}

	return webdiff.Serve(webdiff.Payload{
		FilePath:   path,
		HandlerID:  fc.HandlerID,
		Mode:       "diff",
		Compare:    fmt.Sprintf("%s → %s", base, head),
		DiffJSON:   diffJSON,
		RendererJS: rendererPath,
		Renderer3D: fhr.InstalledRenderer3D(fc.HandlerID),
		Base:       fc.Base,
		Head:       fc.Head,
	}, openBrowser)
}

// ensureRenderer returns the path to the installed renderer bundle for a
// handler, downloading it from a configured source if not already present.
func ensureRenderer(handlerID string) (string, error) {
	if p := fhr.InstalledRenderer(handlerID); p != "" {
		return p, nil
	}
	sources, err := fhr.LoadSources()
	if err != nil {
		return "", err
	}
	if len(sources) == 0 {
		return "", fmt.Errorf("no renderer installed for %q and no sources configured — run: forge source add <url>", handlerID)
	}
	for _, src := range sources {
		m, err := fhr.FetchManifest(src.URL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forge: warning: could not fetch source %q: %v\n", src.Name, err)
			continue
		}
		if _, err := m.RendererAssetURL(handlerID); err != nil {
			continue
		}
		return fhr.DownloadRenderer(m, handlerID, src.URL)
	}
	return "", fmt.Errorf("no renderer for %q found in any configured source", handlerID)
}

// maybeInstallRenderer downloads a handler's renderer bundle if the source
// advertises one. Best effort — a missing renderer only disables `forge diff --web`.
func maybeInstallRenderer(m *fhr.FHRManifest, handlerID, sourceURL string) {
	if _, err := m.RendererAssetURL(handlerID); err != nil {
		return
	}
	if _, err := fhr.DownloadRenderer(m, handlerID, sourceURL); err != nil {
		fmt.Fprintf(os.Stderr, "forge: warning: handler installed but renderer download failed: %v\n", err)
	}
}

func renderDiff(path string, diff handler.StructuredDiff) {
	if len(diff.Changes) == 0 {
		return
	}
	fmt.Printf("\x1b[1m--- a/%s\n+++ b/%s\x1b[0m\n", path, path)
	renderChanges(diff.Changes, "", "")
}

func renderChanges(changes []handler.DiffChange, connPrefix, contPrefix string) {
	n := len(changes)
	for i, c := range changes {
		isLast := i == n-1
		label := c.Label
		if label == "" {
			label = c.Path
		}

		myConn := connPrefix
		childConn := contPrefix + "  "
		childCont := contPrefix + "  "
		if connPrefix != "" {
			if isLast {
				myConn = contPrefix + "└─ "
				childConn = contPrefix + "   "
				childCont = contPrefix + "   "
			} else {
				myConn = contPrefix + "├─ "
				childConn = contPrefix + "│  "
				childCont = contPrefix + "│  "
			}
		}

		if len(c.Children) > 0 {
			switch c.Kind {
			case handler.Added:
				fmt.Printf("\x1b[32m%s+ [%s] %v\x1b[0m\n", myConn, label, c.After)
				renderChanges(c.Children, childConn, childCont)
			case handler.Removed:
				fmt.Printf("\x1b[31m%s- [%s] %v\x1b[0m\n", myConn, label, c.Before)
				renderChanges(c.Children, childConn, childCont)
			default:
				if connPrefix == "" {
					fmt.Printf("\n%s\n", label)
				} else {
					fmt.Printf("%s%s\n", myConn, label)
				}
				renderChanges(c.Children, childConn, childCont)
			}
		} else {
			switch c.Kind {
			case handler.Added:
				fmt.Printf("\x1b[32m%s+ [%s] %v\x1b[0m\n", myConn, label, c.After)
			case handler.Removed:
				fmt.Printf("\x1b[31m%s- [%s] %v\x1b[0m\n", myConn, label, c.Before)
			case handler.Modified:
				fmt.Printf("\x1b[33m%s%s  %v  →  %v\x1b[0m\n", myConn, label, c.Before, c.After)
			}
		}
	}
}

// ── forge mergetool ──────────────────────────────────────────────────────────────────────────────────

func mergeToolCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mergetool [file...]",
		Short: "Resolve merge conflicts using the format-appropriate tool",
		Long: `Opens each conflicted file in a resolution tool and checks the result.

For text files: opens in $MERGE_TOOL, $VISUAL, $EDITOR, or the first
available tool from git's built-in list (meld, vimdiff, vim, vi…).
Conflict is resolved when all <<<<<<< markers have been removed.

For binary formats (handlers installed via forge formats add):
prints the semantic conflict paths so you can resolve them in your
tool of choice, then re-export the file and run 'git add <file>'.

After all conflicts are resolved: run 'git add <file>' and 'git commit'.`,
		Args: cobra.ArbitraryArgs,
		RunE: runMergeTool,
	}
}

func runMergeTool(_ *cobra.Command, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	var files []string
	if len(args) > 0 {
		files = args
	} else {
		files, err = findConflictedFiles(cwd)
		if err != nil {
			return err
		}
	}

	if len(files) == 0 {
		fmt.Println("No files need merging.")
		return nil
	}

	reg := defaultRegistry()
	tool := resolveMergeTool()
	resolved, total := 0, len(files)

	for _, f := range files {
		fmt.Printf("\nMerging %s\n", f)

		h, _ := reg.Resolve(f)
		isBinary := h != nil && forgerepo.IsBinaryHandler(h)

		if isBinary {
			if resolveInteractive(f, h) {
				resolved++
			}
			continue
		}

		if err := openInMergeTool(tool, f); err != nil {
			fmt.Fprintf(os.Stderr, "forge: could not open %s in %q: %v\n", f, tool, err)
			continue
		}
		if hasConflictMarkers(f) {
			fmt.Printf("%s: conflict markers remain — not resolved\n", f)
		} else {
			fmt.Printf("%s: resolved\n", f)
			resolved++
		}
	}

	fmt.Printf("\n%d/%d file(s) resolved.\n", resolved, total)
	if resolved < total {
		fmt.Println("Run 'git add <file>' and 'git commit' once all conflicts are resolved.")
		os.Exit(1)
	}
	fmt.Println("All conflicts resolved. Run 'git add' and 'git commit' to complete the merge.")
	return nil
}

func findConflictedFiles(root string) ([]string, error) {
	seen := make(map[string]bool)
	var conflicted []string

	out, err := exec.Command("git", "ls-files", "-u", "--format=%(path)").Output()
	if err != nil {
		out, _ = exec.Command("git", "ls-files", "-u").Output()
		for _, line := range strings.Split(string(out), "\n") {
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) == 2 {
				p := strings.TrimSpace(parts[1])
				if p != "" && !seen[p] {
					seen[p] = true
					conflicted = append(conflicted, p)
				}
			}
		}
	} else {
		for _, p := range strings.Split(string(out), "\n") {
			p = strings.TrimSpace(p)
			if p != "" && !seen[p] {
				seen[p] = true
				conflicted = append(conflicted, p)
			}
		}
	}

	r, err := gogit.PlainOpenWithOptions(root, &gogit.PlainOpenOptions{DetectDotGit: true})
	if err == nil {
		if wt, err := r.Worktree(); err == nil {
			if st, err := wt.Status(); err == nil {
				for path := range st {
					if seen[path] {
						continue
					}
					abs := filepath.Join(root, filepath.FromSlash(path))
					if hasConflictMarkers(abs) {
						seen[path] = true
						conflicted = append(conflicted, path)
					}
				}
			}
		}
	}

	sort.Strings(conflicted)
	cleanupForgeConflictSidecars(root)

	return conflicted, nil
}

func cleanupForgeConflictSidecars(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".forge-conflict") {
			os.Remove(p)
		}
		return nil
	})
}

func hasConflictMarkers(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Contains(data, []byte("<<<<<<<"))
}

func resolveInteractive(path string, h handler.ForgeHandler) bool {
	sidecarPath := path + ".forge-conflict"
	raw, err := os.ReadFile(sidecarPath)
	if err != nil {
		if mergeErr := runSemanticMergeFromIndex(path, h); mergeErr != nil {
			fmt.Printf("%s: binary conflict — resolve in your tool and re-export.\n", path)
			fmt.Printf("  (%v)\n", mergeErr)
			fmt.Printf("Run 'git add %s' once ready.\n", path)
			return promptConfirm(fmt.Sprintf("Mark %s as resolved?", path))
		}
		raw, err = os.ReadFile(sidecarPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forge: sidecar still missing after on-the-fly merge: %v\n", err)
			return false
		}
	}

	var sc handler.ConflictSidecar
	if err := json.Unmarshal(raw, &sc); err != nil {
		fmt.Fprintf(os.Stderr, "forge: malformed sidecar for %s: %v\n", path, err)
		return false
	}

	applier, canApply := h.(handler.ConflictApplier)
	if !canApply {
		return promptManualResolve(path, sc.Conflicts)
	}

	n := len(sc.Conflicts)
	choices := make([]bool, n)
	idx := 0

	for {
		c := sc.Conflicts[idx]
		dot := func(want bool) string {
			if choices[idx] == want {
				return "●"
			}
			return " "
		}
		fmt.Printf("\n%s  ─  %d conflict(s)   [%d/%d]\n", path, n, idx+1, n)
		fmt.Printf("  path:     %s\n", c.Path)
		fmt.Printf("  ──────────────────────────────────────────\n")
		fmt.Printf("  [c] %s current   %v\n", dot(false), c.Ours)
		fmt.Printf("  [i] %s incoming  %v\n", dot(true), c.Theirs)
		fmt.Printf("\n  c/i = pick · n = next · p = prev · a = apply · q = quit\n")
		fmt.Printf("  > ")

		var input string
		fmt.Scanln(&input)
		switch strings.ToLower(strings.TrimSpace(input)) {
		case "c":
			choices[idx] = false
			if idx < n-1 {
				idx++
			}
		case "i":
			choices[idx] = true
			if idx < n-1 {
				idx++
			}
		case "n":
			if idx < n-1 {
				idx++
			}
		case "p":
			if idx > 0 {
				idx--
			}
		case "a":
			return applyConflictChoices(path, sidecarPath, sc, choices, applier)
		case "q":
			fmt.Println("Aborted — no changes made.")
			return false
		}
	}
}

// promptManualResolve is the route out when forge cannot apply the choices
// itself: the conflicts are printed for the human to act on in their own tool,
// and they say whether the file is resolved.
func promptManualResolve(path string, conflicts []handler.SemanticConflict) bool {
	fmt.Printf("Conflicts in %s:\n", path)
	for _, c := range conflicts {
		fmt.Printf("  %s\n    current:  %v\n    incoming: %v\n", c.Path, c.Ours, c.Theirs)
	}
	fmt.Printf("Resolve in your tool and re-export, then 'git add %s'.\n", path)
	resolved := promptConfirm(fmt.Sprintf("Mark %s as resolved?", path))
	if resolved {
		cleanupMergeTempFiles(path)
	}
	return resolved
}

func applyConflictChoices(path, sidecarPath string, sc handler.ConflictSidecar, choices []bool, applier handler.ConflictApplier) bool {
	fmt.Printf("\nSummary for %s:\n", path)
	var takePaths []string
	for i, c := range sc.Conflicts {
		label, val := "current ", c.Ours
		if choices[i] {
			label, val = "incoming", c.Theirs
			takePaths = append(takePaths, c.Path)
		}
		fmt.Printf("  [%d/%d]  %-45s → %s  %v\n", i+1, len(sc.Conflicts), c.Path, label, val)
	}
	if !promptConfirm("Apply these choices?") {
		fmt.Println("Aborted — no changes made.")
		return false
	}

	merged, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: could not read %s: %v\n", path, err)
		return false
	}
	// Keeping every conflict at current is the merged file already on disk: the
	// merge left the current side wherever it could not reconcile. There is
	// nothing for the handler to apply, so it is not asked — a call that can only
	// return what is already here is one more thing that can fail.
	result := merged
	if len(takePaths) > 0 {
		theirsBlob, err := base64.StdEncoding.DecodeString(sc.TheirsB64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forge: could not decode the incoming side: %v\n", err)
			return false
		}
		if result, err = applier.ApplyChoices(merged, theirsBlob, takePaths); err != nil {
			// The interface says a handler can apply choices; it does not promise the
			// attempt succeeds. Either way the choices could not be applied here, so
			// the same route out is offered as for a handler that never had it.
			fmt.Fprintf(os.Stderr, "forge: %s could not apply these choices: %v\n", sc.Handler, err)
			return promptManualResolve(path, sc.Conflicts)
		}
	}
	if err := os.WriteFile(path, result, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "forge: could not write %s: %v\n", path, err)
		return false
	}
	_ = os.Remove(sidecarPath)
	cleanupMergeTempFiles(path)
	fmt.Printf("%s: resolved.\n", path)
	return true
}

func cleanupMergeTempFiles(path string) {
	dir := filepath.Dir(path)
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	ext := filepath.Ext(path)
	for _, tag := range []string{"_BASE_", "_LOCAL_", "_REMOTE_", "_BACKUP_"} {
		pattern := filepath.Join(dir, base+tag+"*"+ext)
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			os.Remove(m)
		}
	}
}

func promptConfirm(prompt string) bool {
	fmt.Printf("%s [Y/n] ", prompt)
	var answer string
	fmt.Scanln(&answer)
	a := strings.ToLower(strings.TrimSpace(answer))
	return a == "" || a == "y"
}

func runSemanticMergeFromIndex(path string, h handler.ForgeHandler) error {
	readStage := func(stage int) ([]byte, error) {
		data, err := exec.Command("git", "show", fmt.Sprintf(":%d:%s", stage, path)).Output()
		if err != nil {
			return nil, fmt.Errorf("git show :%d:%s: %w", stage, path, err)
		}
		return data, nil
	}

	base, err := readStage(1)
	if err != nil {
		base = nil
	}
	ours, err := readStage(2)
	if err != nil {
		return err
	}
	theirs, err := readStage(3)
	if err != nil {
		return err
	}

	merged, ci, err := h.Merge(base, ours, theirs)
	if err != nil {
		return fmt.Errorf("semantic merge: %w", err)
	}

	if err := os.WriteFile(path, merged, 0644); err != nil {
		return fmt.Errorf("writing merged result: %w", err)
	}

	if ci != nil && len(ci.Conflicts) > 0 {
		format := "unknown"
		if n, ok := h.(interface{ Format() string }); ok {
			format = n.Format()
		}
		sc := handler.ConflictSidecar{
			Handler:   format,
			Conflicts: ci.Conflicts,
			TheirsB64: base64.StdEncoding.EncodeToString(theirs),
		}
		if data, err := json.MarshalIndent(sc, "", "  "); err == nil {
			_ = os.WriteFile(path+".forge-conflict", data, 0644)
		}
	}
	return nil
}

func resolveMergeTool() string {
	for _, env := range []string{"MERGE_TOOL", "GIT_MERGETOOL", "VISUAL", "EDITOR"} {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	builtins := []string{
		"meld", "opendiff", "kdiff3", "tkdiff", "xxdiff",
		"tortoisemerge", "gvimdiff", "diffuse", "diffmerge",
		"ecmerge", "p4merge", "araxis", "bc", "codecompare",
		"smerge", "emerge", "nvimdiff", "nvim", "vimdiff", "vim", "vi",
	}
	for _, t := range builtins {
		if _, err := exec.LookPath(t); err == nil {
			return t
		}
	}
	return "vi"
}

var threeWayTools = map[string]bool{
	"meld": true, "kdiff3": true, "xxdiff": true, "diffuse": true,
	"tkdiff": true, "diffmerge": true, "bc": true, "bcompare": true,
}

func openInMergeTool(tool, path string) error {
	args := []string{path}
	var cleanup []string

	if threeWayTools[filepath.Base(tool)] {
		local, remote, err := extractConflictVersions(path)
		if err == nil {
			args = []string{local, path, remote}
			cleanup = []string{local, remote}
		}
	}

	c := exec.Command(tool, args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	err := c.Run()
	for _, f := range cleanup {
		os.Remove(f)
	}
	return err
}

func extractConflictVersions(path string) (localFile, remoteFile string, err error) {
	localData, err := exec.Command("git", "show", ":2:"+path).Output()
	if err != nil {
		return "", "", fmt.Errorf("git show :2:%s: %w", path, err)
	}
	remoteData, err := exec.Command("git", "show", ":3:"+path).Output()
	if err != nil {
		return "", "", fmt.Errorf("git show :3:%s: %w", path, err)
	}

	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	ext := filepath.Ext(path)

	lf, err := os.CreateTemp("", "forge-LOCAL-"+base+"-*"+ext)
	if err != nil {
		return "", "", err
	}
	defer lf.Close()
	if _, err = lf.Write(localData); err != nil {
		os.Remove(lf.Name())
		return "", "", err
	}

	rf, err := os.CreateTemp("", "forge-REMOTE-"+base+"-*"+ext)
	if err != nil {
		os.Remove(lf.Name())
		return "", "", err
	}
	defer rf.Close()
	if _, err = rf.Write(remoteData); err != nil {
		os.Remove(lf.Name())
		os.Remove(rf.Name())
		return "", "", err
	}

	return lf.Name(), rf.Name(), nil
}

// ── forge merge ────────────────────────────────────────────────────────────────────────────────────

func mergeCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "merge",
		Short:              "Merge a branch, using Forge handlers for supported formats",
		DisableFlagParsing: true,
		RunE:               runMerge,
	}
}

func runMerge(_ *cobra.Command, args []string) error {
	isAbort := false
	for _, a := range args {
		if a == "--abort" {
			isAbort = true
			break
		}
	}

	c := exec.Command("git", append([]string{"merge"}, args...)...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	err := c.Run()

	if isAbort {
		cleanupForgeConflictSidecars(".")
		return err
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "\nTo resolve conflicts run: forge mergetool")
		fmt.Fprintln(os.Stderr, "Then: git add <files> && git commit")
		os.Exit(c.ProcessState.ExitCode())
	}
	return nil
}

// ── forge merge-file ──────────────────────────────────────────────────────────────────────────────────────

func mergeFileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "merge-file <base> <ours> <theirs>",
		Short: "3-way merge three files using the format handler (like git merge-file)",
		Long: `Performs a 3-way merge of BASE, OURS, and THEIRS using the appropriate
format handler. The result is written back to OURS, matching git merge-file behaviour.

Exits 0 on a clean merge, 1 if there are conflicts.`,
		Args: cobra.RangeArgs(3, 4),
		RunE: runMergeFile,
	}
}

func runMergeFile(_ *cobra.Command, args []string) error {
	basePath, oursPath, theirsPath := cleanPath(args[0]), cleanPath(args[1]), cleanPath(args[2])
	sidecarBase := oursPath
	if len(args) == 4 {
		sidecarBase = cleanPath(args[3])
	}

	base, err := os.ReadFile(basePath)
	if err != nil {
		return fmt.Errorf("reading base %s: %w", basePath, err)
	}
	ours, err := os.ReadFile(oursPath)
	if err != nil {
		return fmt.Errorf("reading ours %s: %w", oursPath, err)
	}
	theirs, err := os.ReadFile(theirsPath)
	if err != nil {
		return fmt.Errorf("reading theirs %s: %w", theirsPath, err)
	}

	reg := defaultRegistry()
	h, err := reg.Resolve(oursPath)
	if err != nil {
		return err
	}

	merged, ci, err := h.Merge(base, ours, theirs)
	if err != nil {
		return fmt.Errorf("merge failed: %w", err)
	}

	if err := os.WriteFile(oursPath, merged, 0644); err != nil {
		return fmt.Errorf("writing result to %s: %w", oursPath, err)
	}

	if ci != nil && len(ci.Conflicts) > 0 {
		format := "unknown"
		if n, ok := h.(interface{ Format() string }); ok {
			format = n.Format()
		}
		sidecar := handler.ConflictSidecar{
			Handler:   format,
			Conflicts: ci.Conflicts,
			TheirsB64: base64.StdEncoding.EncodeToString(theirs),
		}
		if data, err := json.MarshalIndent(sidecar, "", "  "); err == nil {
			_ = os.WriteFile(sidecarBase+".forge-conflict", data, 0644)
		}

		fmt.Fprintf(os.Stderr, "CONFLICT: %d conflict(s) in %s\n", len(ci.Conflicts), oursPath)
		for _, c := range ci.Conflicts {
			fmt.Fprintf(os.Stderr, "  %s\n", c.Path)
		}
		os.Exit(1)
	}

	fmt.Printf("Merged cleanly into %s\n", oursPath)
	return nil
}

// ── forge mcp ───────────────────────────────────────────────────────────────────────────────────────

func mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve this repository to an MCP client over stdio",
		Long: `Runs a Model Context Protocol server over stdin and stdout, so an agent can
ask the questions forge can answer — what changed inside a file whose format has
a handler, which handler claims a path, what a commit changed — and complete the
loop those answers are for: decide a semantic conflict, write the resolution,
stage it, commit it.

Writes are served by default. --read-only serves only the tools annotated
read-only, and takes precedence over anything else in a client's configuration.
Either way the source list — forge's trust boundary — is reported but never
modified, nothing is pushed, pulled or fetched, and no tool amends, forces or
resets the working tree.

The repository is the one this command is run in, resolved once at startup;
every path an agent passes is resolved against that root and refused if it
points outside.

stdout carries the protocol, so run it from a client rather than a terminal:

  { "mcpServers": { "forge": { "command": "forge", "args": ["mcp"] } } }`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			readOnly, _ := cmd.Flags().GetBool("read-only")
			return mcpserver.Run(cmd.Context(), readOnly)
		},
	}
	cmd.Flags().Bool("read-only", false, "Serve only the tools that change nothing")
	return cmd
}

// ── forge source ────────────────────────────────────────────────────────────────────────────────────

func sourceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "source",
		Short: "Manage FHR handler sources",
	}
	cmd.AddCommand(sourceAddCmd(), sourceListCmd(), sourceUpdateCmd(), sourceRemoveCmd())
	return cmd
}

func sourceAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <url>",
		Short: "Add a handler source and fetch its manifest",
		Args:  cobra.ExactArgs(1),
		RunE:  runSourceAdd,
	}
	cmd.Flags().String("name", "", "Short name for this source (default: derived from URL)")
	return cmd
}

func runSourceAdd(cmd *cobra.Command, args []string) error {
	rawURL := args[0]
	name, _ := cmd.Flags().GetString("name")
	if name == "" {
		name = deriveSourceName(rawURL)
	}

	fmt.Printf("Fetching manifest from %s...\n", rawURL)
	m, err := fhr.FetchManifest(rawURL)
	if err != nil {
		return err
	}

	if err := fhr.AddSource(name, rawURL); err != nil {
		return err
	}

	var exts []string
	for ext := range m.Formats {
		exts = append(exts, ext)
	}
	sort.Strings(exts)

	fmt.Printf("Added source %q (%s)\n", name, m.Name)
	fmt.Printf("Available formats: %s\n", strings.Join(exts, ", "))
	fmt.Printf("Install a handler with: forge formats add <extension>\n")
	return nil
}

func deriveSourceName(rawURL string) string {
	parts := strings.Split(strings.TrimRight(rawURL, "/"), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		p := parts[i]
		if p != "" && p != "manifest.toml" && p != "main" && p != "master" {
			return p
		}
	}
	return "source"
}

func sourceListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured handler sources",
		RunE: func(_ *cobra.Command, _ []string) error {
			sources, err := fhr.LoadSources()
			if err != nil {
				return err
			}
			if len(sources) == 0 {
				fmt.Println("No sources configured.")
				fmt.Println("Add one with: forge source add <url>")
				return nil
			}
			// Index is a display convenience for `forge source remove <index>`;
			// it is stable only within a single list→remove cycle.
			for i, s := range sources {
				fmt.Printf("%3d  %-20s %s\n", i+1, s.Name, s.URL)
			}
			return nil
		},
	}
}

func sourceUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update [name]",
		Short: "Re-fetch manifests to verify sources are reachable",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			sources, err := fhr.LoadSources()
			if err != nil {
				return err
			}
			if len(sources) == 0 {
				fmt.Println("No sources configured.")
				return nil
			}
			for _, s := range sources {
				if len(args) == 1 && s.Name != args[0] {
					continue
				}
				fmt.Printf("Checking %s (%s)... ", s.Name, s.URL)
				if _, err := fhr.FetchManifest(s.URL); err != nil {
					fmt.Printf("ERROR: %v\n", err)
				} else {
					fmt.Println("OK")
				}
			}
			return nil
		},
	}
}

// resolveSourceSelectors maps each selector (a 1-based list index or a source
// name) to a concrete source name, resolving all against the given snapshot
// before any mutation so index selections don't shift as entries are removed.
// Returns an error on the first out-of-range index or unknown name (removing
// nothing); the result is de-duplicated and preserves selector order.
func resolveSourceSelectors(sources []fhr.Source, args []string) ([]string, error) {
	seen := map[string]bool{}
	var names []string
	for _, sel := range args {
		var match *fhr.Source
		if n, convErr := strconv.Atoi(sel); convErr == nil {
			if n < 1 || n > len(sources) {
				return nil, fmt.Errorf("index %d out of range (1..%d) — run: forge source list", n, len(sources))
			}
			match = &sources[n-1]
		} else {
			for i := range sources {
				if sources[i].Name == sel {
					match = &sources[i]
					break
				}
			}
			if match == nil {
				return nil, fmt.Errorf("source %q not found — run: forge source list", sel)
			}
		}
		if !seen[match.Name] {
			seen[match.Name] = true
			names = append(names, match.Name)
		}
	}
	return names, nil
}

func sourceRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <index|name>...",
		Short: "Remove one or more handler sources by list index or name",
		Args:  cobra.MinimumNArgs(1),
		RunE:  runSourceRemove,
	}
}

func runSourceRemove(_ *cobra.Command, args []string) error {
	sources, err := fhr.LoadSources()
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return fmt.Errorf("no sources configured")
	}

	names, err := resolveSourceSelectors(sources, args)
	if err != nil {
		return err
	}

	for _, name := range names {
		if err := fhr.RemoveSource(name); err != nil {
			return err
		}
		fmt.Printf("Removed source %q\n", name)
	}
	return nil
}

// ── forge formats ────────────────────────────────────────────────────────────────────────────────────

func formatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "formats",
		Short: "Manage format handlers for this repository",
		RunE:  runFormats,
	}
	cmd.AddCommand(formatsAddCmd(), formatsIgnoreCmd(), formatsRemoveCmd(), formatsInstallCmd(), formatsUpdateCmd(), formatsListCmd())
	return cmd
}

func formatsIgnoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ignore <extension>",
		Short: "Mark a format as ignored (tracked by git, no semantic handler)",
		Args:  cobra.ExactArgs(1),
		RunE:  runFormatsIgnore,
	}
}

func runFormatsIgnore(_ *cobra.Command, args []string) error {
	ext := args[0]
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	ext = strings.ToLower(ext)

	repoDir := forgerepo.FindRoot()
	if err := forgerepo.IgnoreInForgeFormats(repoDir, ext); err != nil {
		return fmt.Errorf("updating .forge/formats: %w", err)
	}
	fmt.Printf("Ignoring %s (tracked, no handler). Undo with: forge formats add %s\n", ext, ext)
	return nil
}

func runFormats(_ *cobra.Command, _ []string) error {
	repoDir := forgerepo.FindRoot()
	forgeFormats := forgerepo.LoadForgeFormats(repoDir)
	ignored := forgerepo.LoadIgnoredFormats(repoDir)
	if len(forgeFormats) == 0 && len(ignored) == 0 {
		fmt.Println("No formats configured for this repository.")
		fmt.Println("Add one with: forge formats add <extension>")
		return nil
	}

	installed := map[string]fhr.InstalledMeta{}
	for _, meta := range fhr.LoadInstalledHandlers() {
		for _, ext := range meta.Formats {
			installed[strings.ToLower(ext)] = meta
		}
	}

	exts := make([]string, 0, len(forgeFormats))
	for ext := range forgeFormats {
		exts = append(exts, ext)
	}
	sort.Strings(exts)

	if len(exts) > 0 {
		fmt.Printf("%-12s  %-12s  %s\n", "EXTENSION", "STATUS", "HANDLER")
		for _, ext := range exts {
			if meta, ok := installed[ext]; ok {
				fmt.Printf("%-12s  %-12s  %s (%s)\n", ext, "installed", meta.ID, meta.Build)
			} else {
				fmt.Printf("%-12s  %-12s  (run: forge formats add %s)\n", ext, "missing", ext)
			}
		}
	}

	if len(ignored) > 0 {
		ignoredExts := make([]string, 0, len(ignored))
		for ext := range ignored {
			ignoredExts = append(ignoredExts, ext)
		}
		sort.Strings(ignoredExts)
		if len(exts) > 0 {
			fmt.Println()
		}
		fmt.Printf("Ignored (tracked, no handler): %s\n", strings.Join(ignoredExts, ", "))
	}
	return nil
}

func formatsAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add [extension]",
		Short: "Add a format to this repo and install its handler (no arg: scan the repo)",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runFormatsAdd,
	}
	cmd.Flags().String("source", "", "Source name to search (default: all configured sources)")
	cmd.Flags().Bool("all", false, "With no extension: add every discovered format, even ones with no handler")
	return cmd
}

func runFormatsAdd(cmd *cobra.Command, args []string) error {
	sourceName, _ := cmd.Flags().GetString("source")
	all, _ := cmd.Flags().GetBool("all")
	repoDir := forgerepo.FindRoot()

	sources, err := fhr.LoadSources()
	if err != nil {
		return err
	}
	if sourceName != "" {
		var found fhr.Source
		for _, s := range sources {
			if s.Name == sourceName {
				found = s
				break
			}
		}
		if found.Name == "" {
			return fmt.Errorf("source %q not found — run: forge source list", sourceName)
		}
		sources = []fhr.Source{found}
	}

	// No extension: scan the working tree and bulk-add discovered formats.
	if len(args) == 0 {
		return runFormatsAddBulk(repoDir, sources, all)
	}

	ext := args[0]
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	ext = strings.ToLower(ext)

	if err := forgerepo.AddToForgeFormats(repoDir, ext); err != nil {
		return fmt.Errorf("updating .forge/formats: %w", err)
	}
	if len(sources) == 0 {
		fmt.Printf("Added %s to .forge/formats (inactive — no handler yet).\n", ext)
		fmt.Printf("Install one: forge source add <url>, then re-run: forge formats add %s\n", ext)
		return nil
	}

	resolved, err := resolveAndInstall(repoDir, sources, ext)
	if err != nil {
		return err
	}
	fmt.Printf("Added %s to .forge/formats\n", ext)
	if !resolved {
		fmt.Printf("No handler found for %s in any configured source — recorded but inactive.\n", ext)
		fmt.Printf("  Add a source (forge source add <url>) or ignore it: forge formats ignore %s\n", ext)
	}
	return nil
}

// resolveAndInstall finds a handler for ext among the sources and installs it
// (binary, renderer bundle, and .forge/handlers pin). It does NOT modify
// .forge/formats — the caller decides when to record the extension. Returns
// whether a handler was resolved and installed.
func resolveAndInstall(repoDir string, sources []fhr.Source, ext string) (bool, error) {
	for _, src := range sources {
		m, err := fhr.FetchManifest(src.URL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forge: warning: could not fetch source %q: %v\n", src.Name, err)
			continue
		}
		handlerID, build, err := m.HandlerForExt(ext)
		if err != nil {
			continue
		}
		fmt.Printf("Found handler %q (build %s) in source %q\n", handlerID, build, src.Name)
		if fhr.InstalledHandlerBinary(handlerID) != "" {
			fmt.Printf("Handler %q already installed, skipping download\n", handlerID)
			if err := setupGitMergeDriver(repoDir); err != nil {
				fmt.Fprintf(os.Stderr, "forge: warning: could not update .gitattributes: %v\n", err)
			}
			installedBuild := fhr.InstalledHandlerBuild(handlerID)
			handlers := forgerepo.LoadForgeHandlers(repoDir)
			if _, pinned := handlers[handlerID]; !pinned && installedBuild != "" {
				handlers[handlerID] = &installedBuild
				if err := forgerepo.SaveForgeHandlers(repoDir, handlers); err != nil {
					fmt.Fprintf(os.Stderr, "forge: warning: could not update .forge/handlers: %v\n", err)
				}
			}
			if installedBuild != "" && installedBuild != build {
				fmt.Printf("note: newer handler build available (%s → %s). Run: forge formats update %s\n", installedBuild, build, ext)
			}
			if fhr.InstalledRenderer(handlerID) == "" {
				maybeInstallRenderer(m, handlerID, src.URL)
			}
			return true, nil
		}
		binary, err := fhr.DownloadHandler(m, handlerID, src.URL)
		if err != nil {
			return false, err
		}
		fmt.Printf("Installed: %s\n", binary)
		maybeInstallRenderer(m, handlerID, src.URL)
		if err := setupGitMergeDriver(repoDir); err != nil {
			fmt.Fprintf(os.Stderr, "forge: warning: could not update .gitattributes: %v\n", err)
		}
		handlers := forgerepo.LoadForgeHandlers(repoDir)
		b := build
		handlers[handlerID] = &b
		if err := forgerepo.SaveForgeHandlers(repoDir, handlers); err != nil {
			fmt.Fprintf(os.Stderr, "forge: warning: could not update .forge/handlers: %v\n", err)
		}
		return true, nil
	}
	return false, nil
}

// runFormatsAddBulk scans the working tree for extensions not yet included or
// ignored, and adds the ones a configured source can handle (reporting the
// rest). With all=true it adds every discovered extension regardless.
func runFormatsAddBulk(repoDir string, sources []fhr.Source, all bool) error {
	discovered, err := discoverRepoExtensions(repoDir)
	if err != nil {
		return err
	}
	included := forgerepo.LoadForgeFormats(repoDir)
	ignored := forgerepo.LoadIgnoredFormats(repoDir)

	var candidates []string
	for _, ext := range discovered {
		if !included[ext] && !ignored[ext] {
			candidates = append(candidates, ext)
		}
	}
	if len(candidates) == 0 {
		fmt.Println("No new formats found — every tracked extension is already added or ignored.")
		return nil
	}
	if len(sources) == 0 && !all {
		fmt.Printf("Found unregistered formats: %s\n", strings.Join(candidates, ", "))
		fmt.Println("No handler sources configured — run: forge source add <url> (or: forge formats add --all to add them anyway).")
		return nil
	}

	var added, noHandler []string
	for _, ext := range candidates {
		resolved, err := resolveAndInstall(repoDir, sources, ext)
		if err != nil {
			return err
		}
		if resolved || all {
			if err := forgerepo.AddToForgeFormats(repoDir, ext); err != nil {
				return err
			}
			added = append(added, ext)
		} else {
			noHandler = append(noHandler, ext)
		}
	}

	if len(added) > 0 {
		fmt.Printf("Added %d format(s) to .forge/formats: %s\n", len(added), strings.Join(added, ", "))
	}
	if len(noHandler) > 0 {
		fmt.Printf("Found but no handler (not added): %s\n", strings.Join(noHandler, ", "))
		fmt.Println("  Add explicitly: forge formats add <ext>   ·   ignore: forge formats ignore <ext>")
	}
	return nil
}

// discoverRepoExtensions returns the distinct, lower-cased file extensions of
// the repo's git-tracked files (so build artifacts and .gitignore'd paths are
// excluded). Extension-less files and dotfiles are skipped.
func discoverRepoExtensions(repoDir string) ([]string, error) {
	cmd := exec.Command("git", "ls-files")
	cmd.Dir = repoDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listing tracked files: %w", err)
	}
	seen := map[string]bool{}
	var exts []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.TrimSpace(line)
		if f == "" {
			continue
		}
		base := filepath.Base(f)
		e := strings.ToLower(filepath.Ext(base))
		if e == "" || e == base { // no extension, or a dotfile like .gitignore
			continue
		}
		if !seen[e] {
			seen[e] = true
			exts = append(exts, e)
		}
	}
	sort.Strings(exts)
	return exts, nil
}

func formatsRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <extension>",
		Short: "Remove a format from this repository's format list",
		Args:  cobra.ExactArgs(1),
		RunE:  runFormatsRemove,
	}
}

func runFormatsRemove(_ *cobra.Command, args []string) error {
	ext := args[0]
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	ext = strings.ToLower(ext)

	repoDir := forgerepo.FindRoot()
	if err := forgerepo.RemoveFromForgeFormats(repoDir, ext); err != nil {
		return err
	}
	if err := removeFromGitAttributes(repoDir, ext); err != nil {
		fmt.Fprintf(os.Stderr, "forge: warning: could not update .gitattributes: %v\n", err)
	}
	fmt.Printf("Removed %s from .forge/formats\n", ext)
	return nil
}

func removeFromGitAttributes(repoDir, ext string) error {
	attrPath := filepath.Join(repoDir, ".gitattributes")
	data, err := os.ReadFile(attrPath)
	if err != nil {
		return nil
	}
	prefix := "*" + ext
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			continue
		}
		out = append(out, line)
	}
	return os.WriteFile(attrPath, []byte(strings.Join(out, "\n")), 0644)
}

func formatsUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update [extension]",
		Short: "Update installed handler(s) if a newer build is available",
		Long: "Updates the handlers for this repo's listed formats to the current build,\n" +
			"installing any that are missing. `forge formats install` is the same\n" +
			"operation under its reconcile-focused name.",
		Args: cobra.MaximumNArgs(1),
		RunE: runFormatsUpdate,
	}
}

// formatsInstallCmd is the discoverable name for the reconcile operation that
// `formats update` already performs (issue #34): install a handler for every
// format this repo lists that isn't installed yet — exactly what a fresh clone
// of a repo with a committed .forge/formats needs.
func formatsInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install [extension]",
		Short: "Install handlers for every listed format that is missing",
		Long: "Installs a handler for every format listed in .forge/formats that has no\n" +
			"installed handler yet (and refreshes outdated ones). Run it after cloning a\n" +
			"repo with a committed .forge/formats, or whenever `forge status` reports\n" +
			"formats with no handler. Alias of `forge formats update`.",
		Args: cobra.MaximumNArgs(1),
		RunE: runFormatsUpdate,
	}
}

func runFormatsUpdate(_ *cobra.Command, args []string) error {
	repoDir := forgerepo.FindRoot()
	forgeFormats := forgerepo.LoadForgeFormats(repoDir)

	var targetExts []string
	if len(args) == 1 {
		ext := args[0]
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		targetExts = []string{strings.ToLower(ext)}
	} else {
		for ext := range forgeFormats {
			targetExts = append(targetExts, ext)
		}
		sort.Strings(targetExts)
	}

	if len(targetExts) == 0 {
		fmt.Println("No formats configured. Add one with: forge formats add <extension>")
		return nil
	}

	sources, err := fhr.LoadSources()
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return fmt.Errorf("no sources configured — run: forge source add <url>")
	}

	lockfile := forgerepo.LoadForgeHandlers(repoDir)
	dirty := false

	for _, ext := range targetExts {
		for _, src := range sources {
			m, err := fhr.FetchManifest(src.URL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "forge: warning: could not fetch source %q: %v\n", src.Name, err)
				continue
			}
			handlerID, build, err := m.HandlerForExt(ext)
			if err != nil {
				continue
			}
			pin := lockfile[handlerID]
			if pin != nil && *pin == build {
				fmt.Printf("%s: already up to date (build %s)\n", ext, build)
				break
			}
			if fhr.InstalledHandlerBinary(handlerID) != "" && fhr.InstalledHandlerBuild(handlerID) == build {
				fmt.Printf("%s: already up to date (build %s)\n", ext, build)
				if pin == nil {
					b := build
					lockfile[handlerID] = &b
					dirty = true
				}
				break
			}
			fmt.Printf("Updating %s handler (%s, build %s)...\n", ext, handlerID, build)
			binary, err := fhr.DownloadHandler(m, handlerID, src.URL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "forge: error updating %s: %v\n", ext, err)
				continue
			}
			fmt.Printf("Updated: %s\n", binary)
			maybeInstallRenderer(m, handlerID, src.URL)
			b := build
			lockfile[handlerID] = &b
			dirty = true
			break
		}
	}

	if dirty {
		if err := forgerepo.SaveForgeHandlers(repoDir, lockfile); err != nil {
			fmt.Fprintf(os.Stderr, "forge: warning: could not save .forge/handlers: %v\n", err)
		}
	}
	return nil
}

func formatsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List globally installed format handlers",
		RunE: func(_ *cobra.Command, _ []string) error {
			handlers := fhr.LoadInstalledHandlers()
			if len(handlers) == 0 {
				fmt.Println("No external handlers installed.")
				fmt.Println("Install one with: forge formats add <extension>")
				return nil
			}
			fmt.Printf("%-20s %-12s %s\n", "HANDLER", "BUILD", "FORMATS")
			for _, h := range handlers {
				sort.Strings(h.Formats)
				fmt.Printf("%-20s %-12s %s\n", h.ID, h.Build, strings.Join(h.Formats, ", "))
			}
			return nil
		},
	}
}

// ── git pass-throughs ────────────────────────────────────────────────────────────────────────────────────

func gitPassthrough(name, short string) *cobra.Command {
	return &cobra.Command{
		Use:                name,
		Short:              short,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			c := exec.Command("git", append([]string{name}, args...)...)
			c.Stdin = os.Stdin
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			return c.Run()
		},
	}
}

func logCmd() *cobra.Command {
	return gitPassthrough("log", "Show commit log (delegates to git)")
}

func pushCmd() *cobra.Command {
	return gitPassthrough("push", "Push to remote (delegates to git)")
}

func pullCmd() *cobra.Command {
	return gitPassthrough("pull", "Pull from remote (delegates to git)")
}

func delegateToGit(sub string) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, args []string) error {
		c := exec.Command("git", append([]string{sub}, args...)...)
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	}
}
