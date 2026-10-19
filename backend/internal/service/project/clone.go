package project

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

var scpStyleGitURL = regexp.MustCompile(`^[^/@:\s]+@[^/:\s]+:(.+)$`)

var allowedCloneSchemes = map[string]struct{}{
	"file":  {},
	"git":   {},
	"http":  {},
	"https": {},
	"ssh":   {},
}

const clonePreparationMarker = ".ao-clone-prepared"

// maxCloneStderrTail bounds the git stderr tail carried in an error envelope.
// Git progress output can be megabytes for a large clone; the diagnosis lives
// in the last lines (fatal error, credential hint, network failure).
const maxCloneStderrTail = 4000

// Clone checks out one remote repository into a user-selected parent folder,
// then registers it through the same Add boundary as an existing local repo.
// The checkout is staged in a unique sibling directory so a failed or cancelled
// clone can never leave a half-populated destination behind.
func (m *Service) Clone(ctx context.Context, in CloneInput) (Project, error) {
	prepared, err := m.prepareClone(ctx, in, false)
	if err != nil {
		return Project{}, err
	}
	if !repoHasCommit(ctx, prepared.Path) {
		m.cleanupPreparedCloneAfterFailure(ctx, prepared)
		return Project{}, apierr.Invalid("CLONE_EMPTY_REPOSITORY", "AO needs a repository with at least one commit.", nil)
	}

	project, err := m.Add(ctx, AddInput{
		Path:               prepared.Path,
		ProjectID:          in.ProjectID,
		Name:               in.Name,
		Config:             in.Config,
		ClonePreparationID: prepared.PreparationID,
	})
	if err != nil {
		m.cleanupPreparedCloneAfterFailure(ctx, prepared)
		return Project{}, err
	}
	return project, nil
}

// PrepareClone checks out a repository without registering it as a project.
func (m *Service) PrepareClone(ctx context.Context, in CloneInput) (ClonePreparationResult, error) {
	return m.prepareClone(ctx, in, true)
}

// SubscribeCloneProgress exposes the live clone progress hub to the SSE
// surface. Events are best-effort broadcasts; see clone_progress.go.
func (m *Service) SubscribeCloneProgress(ctx context.Context) <-chan CloneProgressEvent {
	return m.cloneProgress.subscribe(ctx)
}

// CleanupPreparedClone removes an unregistered checkout created by PrepareClone.
func (m *Service) CleanupPreparedClone(ctx context.Context, in ClonePreparationCleanupInput) error {
	path, err := normalizePath(in.Path)
	if err != nil {
		return err
	}
	if err := validateRepositorySetupPathSafety(path); err != nil {
		return err
	}
	m.addMu.Lock()
	defer m.addMu.Unlock()

	markerID, exists, err := readClonePreparationID(path)
	if !exists && err == nil {
		return nil
	} else if err != nil {
		return apierr.Invalid("CLONE_CLEANUP_FAILED", "The prepared clone could not be inspected.", map[string]any{"path": path})
	}
	if !sameClonePreparationID(markerID, in.PreparationID) {
		return apierr.Conflict("CLONE_PREPARATION_MISMATCH", "This checkout belongs to a different clone preparation.", map[string]any{"path": path})
	}
	if _, registered, err := m.store.FindProjectByPath(ctx, path); err != nil {
		return apierr.Invalid("CLONE_CLEANUP_FAILED", "The prepared clone registration state could not be inspected.", map[string]any{"path": path})
	} else if registered {
		removeClonePreparationMarker(path)
		return nil
	}
	if err := os.RemoveAll(path); err != nil {
		return apierr.Invalid("CLONE_CLEANUP_FAILED", "The abandoned prepared clone could not be removed.", map[string]any{"path": path})
	}
	return nil
}

func (m *Service) cleanupPreparedCloneAfterFailure(ctx context.Context, prepared ClonePreparationResult) {
	if err := m.CleanupPreparedClone(context.WithoutCancel(ctx), ClonePreparationCleanupInput{
		Path: prepared.Path, PreparationID: prepared.PreparationID,
	}); err != nil {
		m.logger.Warn("project: failed to clean up prepared clone", "path", prepared.Path, "error", err)
	}
}

func (m *Service) prepareClone(ctx context.Context, in CloneInput, resume bool) (ClonePreparationResult, error) {
	remoteURL := strings.TrimSpace(in.RemoteURL)
	repositoryName, err := cloneRepositoryName(remoteURL)
	if err != nil {
		return ClonePreparationResult{}, err
	}
	parent, err := normalizePath(in.DestinationParent)
	if err != nil {
		return ClonePreparationResult{}, err
	}
	if in.Config != nil {
		if err := in.Config.Validate(); err != nil {
			return ClonePreparationResult{}, apierr.Invalid("INVALID_PROJECT_CONFIG", err.Error(), nil)
		}
	}
	if in.ProjectID != nil {
		if err := validateProjectID(domain.ProjectID(strings.TrimSpace(*in.ProjectID))); err != nil {
			return ClonePreparationResult{}, err
		}
	}

	target := filepath.Join(parent, repositoryName)
	if err := validateRepositorySetupPathSafety(target); err != nil {
		return ClonePreparationResult{}, err
	}
	if info, err := os.Lstat(target); err == nil {
		if resume && info.IsDir() {
			markerID, exists, markerErr := readClonePreparationID(target)
			if markerErr == nil && exists {
				storedURL, gitErr := aoprocess.CommandContext(ctx, "git", "-C", target, "config", "--local", "--get", "remote.origin.url").Output()
				if gitErr == nil && strings.TrimSpace(string(storedURL)) == remoteURL {
					if _, registered, storeErr := m.store.FindProjectByPath(ctx, target); storeErr != nil {
						return ClonePreparationResult{}, apierr.Internal("PROJECT_LOAD_FAILED", "Failed to inspect existing project")
					} else if !registered {
						return ClonePreparationResult{Path: target, RemoteURL: remoteURL, PreparationID: markerID}, nil
					}
				}
			}
		}
		return ClonePreparationResult{}, apierr.Conflict("CLONE_DESTINATION_EXISTS", "A folder with this repository name already exists in the selected destination.", map[string]any{"path": target})
	} else if !errors.Is(err, os.ErrNotExist) {
		return ClonePreparationResult{}, apierr.Invalid("CLONE_DESTINATION_UNAVAILABLE", "The clone destination could not be inspected.", map[string]any{"path": target})
	}

	if err := os.MkdirAll(parent, 0o750); err != nil {
		return ClonePreparationResult{}, apierr.Invalid("CLONE_DESTINATION_UNAVAILABLE", "AO could not create the destination folder.", nil)
	}
	temporaryPath, err := os.MkdirTemp(parent, ".ao-clone-")
	if err != nil {
		return ClonePreparationResult{}, apierr.Invalid("CLONE_DESTINATION_UNAVAILABLE", "AO could not prepare the selected clone destination.", nil)
	}
	cleanupPath := temporaryPath
	defer func() {
		if cleanupPath != "" {
			_ = os.RemoveAll(cleanupPath)
		}
	}()

	// --progress forces git to emit its usual stderr progress meter even
	// without a terminal, which is what the clone progress stream relays.
	cmd := aoprocess.CommandContext(ctx, "git", "clone", "--origin", "origin", "--progress", "--", remoteURL, temporaryPath)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=Never")
	// Kill git's whole process tree on cancellation: a killed `git clone`
	// leaves transport children holding the stderr pipe, which would block
	// the progress scanner (and the error envelope) until they exit.
	aoprocess.ConfigureTreeCancellation(cmd)
	stderrTail, runErr := m.runCloneCommand(remoteURL, cmd)
	if runErr != nil {
		var executableError *exec.Error
		if errors.As(runErr, &executableError) {
			return ClonePreparationResult{}, apierr.Invalid("GIT_NOT_FOUND", "Git is required to clone a repository.", nil)
		}
		// Git's own output is the only real diagnosis for a failed or
		// cancelled clone (bad URL, auth prompt, network trouble, a request
		// budget that fired mid-transfer). Carry a bounded tail in the
		// envelope instead of returning a bare message.
		var details map[string]any
		if trimmed := redactGitCredentials(strings.TrimSpace(stderrTail)); trimmed != "" {
			details = map[string]any{"gitStderr": trimmed}
		}
		failed := CloneProgressEvent{RemoteURL: remoteURL, Stage: CloneStageFailed}
		if ctx.Err() != nil {
			failed.Message = "Repository cloning was cancelled."
			m.cloneProgress.publish(failed)
			return ClonePreparationResult{}, apierr.Invalid("GIT_CLONE_CANCELLED", failed.Message, details)
		}
		failed.Message = "Could not clone this repository. Check the URL, your Git credentials, and your network connection."
		m.cloneProgress.publish(failed)
		return ClonePreparationResult{}, apierr.Invalid("GIT_CLONE_FAILED", failed.Message, details)
	}
	if ctx.Err() != nil {
		return ClonePreparationResult{}, apierr.Invalid("GIT_CLONE_CANCELLED", "Repository cloning was cancelled.", nil)
	}
	done := 100.0
	m.cloneProgress.publish(CloneProgressEvent{RemoteURL: remoteURL, Stage: CloneStageDone, Percent: &done, Message: "Clone complete"})
	preparationID, err := newClonePreparationID()
	if err != nil {
		return ClonePreparationResult{}, apierr.Internal("CLONE_PREPARATION_FAILED", "AO could not identify the prepared clone")
	}
	if err := os.WriteFile(filepath.Join(temporaryPath, ".git", clonePreparationMarker), []byte(preparationID+"\n"), 0o600); err != nil {
		return ClonePreparationResult{}, apierr.Invalid("CLONE_PREPARATION_FAILED", "AO could not mark the prepared clone for cleanup.", nil)
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return ClonePreparationResult{}, apierr.Conflict("CLONE_DESTINATION_EXISTS", "The clone destination became unavailable before the repository could be created.", map[string]any{"path": target})
	}
	cleanupPath = target
	cleanupPath = ""
	return ClonePreparationResult{Path: target, RemoteURL: remoteURL, PreparationID: preparationID}, nil
}

// runCloneCommand runs one git clone, streaming its stderr as clone progress
// events and returning a bounded tail of that output alongside the run error
// so a failure envelope can carry git's own diagnosis.
func (m *Service) runCloneCommand(remoteURL string, cmd *exec.Cmd) (string, error) {
	stderr, pipeErr := cmd.StderrPipe()
	if pipeErr != nil {
		// StderrPipe only fails for an already-started command; fall back to
		// a plain run with no progress stream.
		return "", cmd.Run()
	}
	var tail cloneStderrTail
	if err := cmd.Start(); err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)
	for scanner.Scan() {
		line := scanner.Text()
		tail.add(line)
		if event, ok := cloneProgressFromGitLine(remoteURL, line); ok {
			m.cloneProgress.publish(event)
		}
	}
	// Drain anything the scanner did not consume so Wait never deadlocks on
	// a full pipe.
	_, _ = io.Copy(io.Discard, stderr)
	waitErr := cmd.Wait()
	return tail.value(), waitErr
}

// gitURLCredentialPattern matches scheme://user:password@ so a failed auth
// cannot leak the secret through the error envelope. Git prints the remote
// URL verbatim in auth failures, including any embedded credentials.
var gitURLCredentialPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)([^\s:@/]+):([^\s@/]+)@`)

func redactGitCredentials(stderr string) string {
	return gitURLCredentialPattern.ReplaceAllString(stderr, "${1}${2}:***@")
}

// gitProgressStages maps a stderr progress label to a clone progress stage.
var gitProgressStages = []struct {
	prefix string
	stage  string
}{
	{"Receiving objects:", CloneStageReceiving},
	{"Resolving deltas:", CloneStageResolving},
	{"Updating files:", CloneStageCheckingOut},
	{"Checking out files:", CloneStageCheckingOut},
	{"Cloning into", CloneStageStarting},
}

var gitPercentPattern = regexp.MustCompile(`(\d+)%`)

// cloneProgressFromGitLine parses one git stderr line into a progress event.
// Auxiliary lines (remote: counters, unrelated notices) yield ok=false: the
// stderr tail in the error envelope already preserves them for failures.
func cloneProgressFromGitLine(remoteURL, line string) (CloneProgressEvent, bool) {
	stage := ""
	for _, entry := range gitProgressStages {
		if strings.HasPrefix(line, entry.prefix) {
			stage = entry.stage
			break
		}
	}
	if stage == "" {
		return CloneProgressEvent{}, false
	}
	event := CloneProgressEvent{RemoteURL: remoteURL, Stage: stage, Message: line}
	if match := gitPercentPattern.FindStringSubmatch(line); match != nil {
		if percent, err := strconv.ParseFloat(match[1], 64); err == nil {
			event.Percent = &percent
		}
	}
	return event, true
}

// cloneStderrTail keeps the last lines of git's stderr, bounded so a huge
// clone's progress meter cannot balloon an error envelope.
type cloneStderrTail struct {
	lines []string
}

func (t *cloneStderrTail) add(line string) {
	t.lines = append(t.lines, line)
	if len(t.lines) > 40 {
		t.lines = t.lines[len(t.lines)-40:]
	}
}

func (t *cloneStderrTail) value() string {
	joined := strings.Join(t.lines, "\n")
	if len(joined) > maxCloneStderrTail {
		joined = joined[len(joined)-maxCloneStderrTail:]
	}
	return joined
}

func newClonePreparationID() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func readClonePreparationID(path string) (string, bool, error) {
	marker := filepath.Join(path, ".git", clonePreparationMarker)
	info, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", false, errors.New("clone preparation marker is not a regular file")
	}
	value, err := os.ReadFile(marker)
	if err != nil {
		return "", false, err
	}
	id := strings.TrimSpace(string(value))
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 32 {
		return "", false, errors.New("invalid clone preparation marker")
	}
	return id, true, nil
}

func sameClonePreparationID(actual, requested string) bool {
	actualBytes := []byte(actual)
	requestedBytes := []byte(strings.TrimSpace(requested))
	return len(actualBytes) == len(requestedBytes) && subtle.ConstantTimeCompare(actualBytes, requestedBytes) == 1
}

func removeClonePreparationMarker(path string) {
	_ = os.Remove(filepath.Join(path, ".git", clonePreparationMarker))
}

func cloneRepositoryName(raw string) (string, error) {
	if raw == "" || strings.ContainsAny(raw, "\r\n\t ") || strings.HasPrefix(raw, "-") {
		return "", invalidCloneURL()
	}

	var remotePath string
	if match := scpStyleGitURL.FindStringSubmatch(raw); len(match) == 2 {
		remotePath = match[1]
	} else {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme == "" {
			return "", invalidCloneURL()
		}
		if _, ok := allowedCloneSchemes[strings.ToLower(parsed.Scheme)]; !ok {
			return "", invalidCloneURL()
		}
		scheme := strings.ToLower(parsed.Scheme)
		hasPassword := false
		if parsed.User != nil {
			_, hasPassword = parsed.User.Password()
		}
		if ((scheme == "http" || scheme == "https") && (parsed.User != nil || parsed.RawQuery != "")) || hasPassword {
			return "", apierr.Invalid("GIT_URL_CONTAINS_CREDENTIALS", "Use your configured Git credentials or an SSH URL instead of putting credentials in the repository URL.", nil)
		}
		remotePath = parsed.Path
	}

	remotePath = strings.TrimRight(remotePath, "/\\")
	name := strings.TrimSuffix(filepath.Base(remotePath), ".git")
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\\<>:"|?*`) {
		return "", invalidCloneURL()
	}
	return name, nil
}

func invalidCloneURL() error {
	return apierr.Invalid("INVALID_GIT_URL", "Enter a valid HTTPS, SSH, Git, or file repository URL.", nil)
}
