package main

// export.go implements `lorri export <shell>`.
//
// This is the command the shell hook calls on every prompt. It:
//
//  1. Finds the lorri project for the current directory (by walking up).
//  2. Reads env.json from the project's GC root.
//  3. Applies the env changes against the current ambient environment,
//     storing per-var revert state in SQLite keyed by session ID.
//  4. Emits shell export/unset commands for the diff.
//  5. On leaving a project directory, reverts and cleans up session state.
//
// Session state is tracked via three env vars:
//
//	LORRI_SESSION_ID  — random 64-bit value as base64url, generated once
//	LORRI_PROJECT     — shell.nix path of the currently loaded project
//	LORRI_ENV_HASH    — hash of the env.json that was applied

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	direnv "github.com/nix-community/lorri/direnv_vendor"
)

// opExport is the implementation of `lorri export <shell>`.
//
// The special shell name "direnv-adapter" is handled separately: it emits
// watch_file lines + bash export/unset commands for direnv to eval, without
// any session state management. This is for users who need direnv for editor
// plugin support (emacs-direnv, vscode-direnv, etc.) but want lorri to manage
// the environment data. Their .envrc contains: eval "$(lorri export direnv-adapter)"
func opExport(shellName string) error {
	if shellName == "direnv-adapter" {
		return opExportDirenvAdapter()
	}
	sh, ok := direnv.Shells[shellName]
	if !ok {
		return exitUserError(fmt.Sprintf("export: unsupported shell %q", shellName), nil)
	}
	paths, err := mustInitPaths()
	if err != nil {
		return err
	}
	db, err := OpenLorriDB(paths.SQLiteDB.String())
	if err != nil {
		return err
	}
	defer db.Close()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	project, err := db.FindRegisteredProjectForDir(cwd)
	if err != nil {
		return err
	}

	ambient := ambientEnv()
	base := ambientEnv()
	previous := make(map[string]*string)
	id := os.Getenv("LORRI_SESSION_ID")
	owned := false
	if id != "" {
		session, err := db.GetShellSession(id)
		if err != nil {
			return err
		}
		if session == nil {
			return fmt.Errorf("missing shell restoration state; open a fresh shell")
		}
		owned = session.ShellPID == nativeShellPID()
		previous, err = db.GetEnvPrev(id)
		if err != nil {
			return err
		}
		for name, value := range previous {
			if value == nil {
				delete(base, name)
			} else {
				base[name] = *value
			}
		}
	}
	output := make(direnv.ShellExport)
	if project == nil {
		if id == "" {
			return nil
		}
		for name, value := range previous {
			if value == nil {
				output.Remove(name)
			} else {
				output.Add(name, *value)
			}
		}
		if owned {
			if err := db.DeleteShellSession(id); err != nil {
				return err
			}
		}
		output.Remove("LORRI_SESSION_ID")
		output.Remove("LORRI_PROJECT")
		output.Remove("LORRI_ENV_HASH")
	} else {
		envPath, err := gcRootEnvJSON(project.NixFile, paths)
		if err != nil {
			// Keep the old environment while the new one is prepared. Preserve
			// the registered project type when requesting a background build.
			_ = opPingWithRebuild(paths, project.ProjectFile(), RebuildOnlyIfNotYetWatching)
			if id != "" && os.Getenv("LORRI_PROJECT") != project.NixFile {
				for name, value := range previous {
					if value == nil {
						output.Remove(name)
					} else {
						output.Add(name, *value)
					}
				}
				output.Remove("LORRI_SESSION_ID")
				output.Remove("LORRI_PROJECT")
				output.Remove("LORRI_ENV_HASH")
				rendered, renderErr := sh.Export(output)
				if renderErr != nil {
					return renderErr
				}
				fmt.Print(rendered)
				if owned {
					if deleteErr := db.DeleteShellSession(id); deleteErr != nil {
						return deleteErr
					}
				}
			}
			return fmt.Errorf("environment pending for %s: %w", project.NixFile, err)
		}
		environment, err := readEnvJSON(envPath)
		if err != nil {
			return err
		}
		extra, err := db.ProjectEnvFile(project.NixFile)
		if err != nil {
			return err
		}
		if extra != "" {
			overlay, err := readEnvJSON(extra)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("project environment file: %w", err)
			}
			if err == nil {
				if overlay.Version != 1 {
					return fmt.Errorf("unsupported environment file version %d", overlay.Version)
				}
				environment.Env = append(environment.Env, overlay.Env...)
			}
		}
		if environment.Version != 1 {
			return fmt.Errorf("unsupported environment version %d", environment.Version)
		}
		data, err := json.Marshal(environment)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		hash := base64.RawURLEncoding.EncodeToString(sum[:])
		if owned && project.NixFile == os.Getenv("LORRI_PROJECT") && hash == os.Getenv("LORRI_ENV_HASH") {
			return nil
		}
		// Compose against the restored baseline, never the currently loaded
		// environment. Include removed variables in the outgoing revert diff.
		for name, value := range previous {
			if value == nil {
				output.Remove(name)
			} else {
				output.Add(name, *value)
			}
		}
		prev := make(map[string]*string)
		effective := make(map[string]string)
		for name, value := range base {
			effective[name] = value
		}
		for _, change := range environment.Env {
			if change.Op == "pass" {
				continue
			}
			if change.Name == "" || strings.ContainsAny(change.Name, "=\x00") || strings.HasPrefix(change.Name, "LORRI_") {
				return fmt.Errorf("invalid environment variable %q", change.Name)
			}
			switch change.Op {
			case "set", "unset", "prepend", "append":
			default:
				return fmt.Errorf("invalid environment operation %q", change.Op)
			}
			if _, exists := prev[change.Name]; !exists {
				if value, exists := base[change.Name]; exists {
					v := value
					prev[change.Name] = &v
				} else {
					prev[change.Name] = nil
				}
			}
			applyChange(change, effective, output)
			if value := output[change.Name]; value == nil {
				delete(effective, change.Name)
			} else {
				effective[change.Name] = *value
			}
		}
		if !owned {
			id, err = newSessionID()
			if err != nil {
				return err
			}
		}
		if err := db.SaveSessionAndEnvPrev(id, project.NixFile, hash, prev); err != nil {
			return err
		}
		output.Add("LORRI_SESSION_ID", id)
		output.Add("LORRI_PROJECT", project.NixFile)
		output.Add("LORRI_ENV_HASH", hash)
		// Avoid printing unchanged variable values or credentials.
		for name, value := range output {
			if value != nil && ambient[name] == *value {
				if _, ok := ambient[name]; ok {
					delete(output, name)
				}
			}
		}
	}
	rendered, err := sh.Export(output)
	if err != nil {
		return err
	}
	fmt.Print(rendered)
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newSessionID generates a random 64-bit value encoded as base64url (no padding).
func newSessionID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// gcRootEnvJSON returns the path to env.json for a given project's GC root.
func gcRootEnvJSON(project string, paths *Paths) (string, error) {
	projectFile := NewShellNixProjectFile(mustAbsPath(project))
	gcRootLink := gcRootPathForProject(paths.GCRootDir, projectFile).String()
	target, err := os.Readlink(gcRootLink)
	if err != nil {
		return "", fmt.Errorf("gc root not ready for %s: %w", project, err)
	}
	return filepath.Join(target, "env.json"), nil
}

// readEnvJSON reads and parses env.json from a store path.
func readEnvJSON(path string) (*LorriEnv, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read env.json: %w", err)
	}
	defer f.Close()
	var env LorriEnv
	if err := json.NewDecoder(f).Decode(&env); err != nil {
		return nil, fmt.Errorf("parse env.json: %w", err)
	}
	return &env, nil
}

// ambientEnv returns the current environment as a map.
func ambientEnv() map[string]string {
	env := make(map[string]string)
	for _, kv := range os.Environ() {
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			continue
		}
		env[kv[:idx]] = kv[idx+1:]
	}
	return env
}

// applyChange converts one EnvChange into a ShellExport entry, applying
// prepend/append semantics against the ambient environment.
func applyChange(change EnvChange, ambient map[string]string, export direnv.ShellExport) {
	switch change.Op {
	case "pass":
		// Deliberately leave the ambient value untouched — no-op.
	case "unset":
		export.Remove(change.Name)
	case "set":
		export.Add(change.Name, change.Value)
	case "prepend":
		existing := ambient[change.Name]
		if existing == "" {
			export.Add(change.Name, change.Value)
		} else {
			export.Add(change.Name, change.Value+change.Sep+existing)
		}
	case "append":
		existing := ambient[change.Name]
		if existing == "" {
			export.Add(change.Name, change.Value)
		} else {
			export.Add(change.Name, existing+change.Sep+change.Value)
		}
	}
}

// Native hooks provide $$ because a command substitution can add a process.
func nativeShellPID() int {
	if pid, err := strconv.Atoi(os.Getenv("LORRI_SHELL_PID")); err == nil && pid > 0 {
		return pid
	}
	return os.Getppid()
}

// opExportDirenvAdapter implements `lorri export direnv-adapter`.
//
// Emits watch_file lines + bash export/unset commands for direnv to eval.
// No session state management — direnv owns the env lifecycle here; lorri
// only provides the data. The user's .envrc should contain:
//
//	eval "$(lorri export direnv-adapter)"
func opExportDirenvAdapter() error {
	paths, err := mustInitPaths()
	if err != nil {
		return err
	}

	// Find the lorri project for the current directory.
	db, err := OpenLorriDB(paths.SQLiteDB.String())
	if err != nil {
		return fmt.Errorf("export direnv-adapter: open db: %w", err)
	}
	defer db.Close()

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("export direnv-adapter: getwd: %w", err)
	}

	foundDBProj, err := db.FindRegisteredProjectForDir(cwd)
	if err != nil {
		return fmt.Errorf("export direnv-adapter: %w", err)
	}
	project := ""
	if foundDBProj != nil {
		project = foundDBProj.NixFile
	}

	socketPath := string(paths.DaemonSocketFile)

	if project == "" {
		// No registered project — just watch the socket.
		fmt.Fprintf(os.Stdout, "watch_file %q\n", socketPath)
		return nil
	}

	gcRootPath := gcRootPathForProject(paths.GCRootDir, NewShellNixProjectFile(mustAbsPath(project)))
	storePath, _ := os.Readlink(string(gcRootPath))
	envJSONPath := filepath.Join(storePath, "env.json")

	if storePath == "" || !fileExists(envJSONPath) {
		// Not yet built — watch socket so direnv re-evals when done.
		_ = opPingWithRebuild(paths, NewShellNixProjectFile(mustAbsPath(project)), RebuildAlways)
		fmt.Fprintf(os.Stdout, "watch_file %q\n", socketPath)
		fmt.Fprintln(os.Stderr, "lorri: has not completed an evaluation for this project yet")
		return nil
	}

	lorriEnv, err := readEnvJSON(envJSONPath)
	if err != nil {
		return fmt.Errorf("export direnv-adapter: %w", err)
	}

	ambient := ambientEnv()
	export := make(direnv.ShellExport)
	for _, change := range lorriEnv.Env {
		applyChange(change, ambient, export)
	}

	shellScript, err := direnv.Shells["bash"].Export(export)
	if err != nil {
		return fmt.Errorf("export direnv-adapter: render: %w", err)
	}

	// watch_file on socket + GC root so direnv re-evals when a new build lands.
	fmt.Fprintf(os.Stdout, "watch_file %q\nwatch_file %q\n%s\n",
		socketPath, string(gcRootPath), shellScript)

	_ = opPingWithRebuild(paths, NewShellNixProjectFile(mustAbsPath(project)), RebuildOnlyIfNotYetWatching)
	return nil
}

// isNixInternal returns true for variables that are Nix build internals and
// not meaningful to show in the summary.
func isNixInternal(name string) bool {
	if strings.HasPrefix(name, "NIX_") ||
		strings.HasPrefix(name, "orig") ||
		strings.HasPrefix(name, "__") {
		return true
	}
	switch name {
	case "stdenv", "builder", "system", "out", "outputs", "name",
		"buildInputs", "nativeBuildInputs", "propagatedBuildInputs",
		"propagatedNativeBuildInputs", "buildPhase", "phases",
		"preferLocalBuild", "allowSubstitutes", "doCheck", "doInstallCheck",
		"extraClosure", "origExtraClosure", "strictDeps",
		"depsBuildBuild", "depsBuildBuildPropagated",
		"depsBuildTarget", "depsBuildTargetPropagated",
		"depsHostHost", "depsHostHostPropagated",
		"depsTargetTarget", "depsTargetTargetPropagated",
		"SOURCE_DATE_EPOCH", "RUN_TIME_CLOSURE":
		return true
	}
	return false
}
