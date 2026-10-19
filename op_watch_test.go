package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Watch uses the existing Ping protocol: force the first build, but only
// restore a missing daemon-side watcher on subsequent registrations.
func TestWatchRebuildPolicy(t *testing.T) {
	for _, flake := range []bool{false, true} {
		name := "shell"
		if flake {
			name = "flake"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			t.Setenv("LORRI_SHELL", "bash")
			paths := &Paths{
				SQLiteDB:         mustAbsPath(filepath.Join(dir, "lorri.sqlite")),
				DaemonSocketFile: mustAbsPath(filepath.Join(dir, "daemon.socket")),
			}
			project := NewShellNixProjectFile(mustAbsPath(filepath.Join(dir, "shell.nix")))
			if flake {
				project = NewFlakeProjectFile(mustAbsPath(dir), "devShells.test.default")
			}
			if err := os.WriteFile(nixFilePathForProject(project), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}

			listener, lock, err := NewSocketPath(paths.DaemonSocketFile).Bind()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close(); lock.Release() })

			type pingResult struct {
				request PingRequest
				err     error
			}
			for _, want := range []Rebuild{RebuildAlways, RebuildOnlyIfNotYetWatching} {
				result := make(chan pingResult, 1)
				go func() {
					conn, err := listener.Accept()
					if err != nil {
						result <- pingResult{err: err}
						return
					}
					framing := NewFraming(conn)
					defer framing.Close()
					var comm CommunicationType
					if err := framing.ReadMsg(defaultReadTimeout, &comm); err != nil {
						result <- pingResult{err: err}
						return
					}
					if comm != CommPing {
						result <- pingResult{err: fmt.Errorf("communication type = %q, want Ping", comm)}
						return
					}
					if err := framing.WriteMsg(defaultReadTimeout, ConnectionAccepted{}); err != nil {
						result <- pingResult{err: err}
						return
					}
					var request PingRequest
					err = framing.ReadMsg(defaultReadTimeout, &request)
					result <- pingResult{request: request, err: err}
				}()

				if err := opWatch(paths, project); err != nil {
					t.Fatalf("opWatch: %v", err)
				}
				select {
				case got := <-result:
					if got.err != nil {
						t.Fatal(got.err)
					}
					if got.request.Rebuild != want {
						t.Fatalf("rebuild policy = %q, want %q", got.request.Rebuild, want)
					}
					if !reflect.DeepEqual(got.request.ProjectFile, project) {
						t.Fatalf("project = %#v, want %#v", got.request.ProjectFile, project)
					}
				case <-time.After(3 * time.Second):
					t.Fatalf("opWatch did not send a %s ping", want)
				}
			}
		})
	}
}

func TestWatchWithoutDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("LORRI_SHELL", "bash")
	paths := &Paths{
		SQLiteDB:         mustAbsPath(filepath.Join(dir, "lorri.sqlite")),
		DaemonSocketFile: mustAbsPath(filepath.Join(dir, "missing.socket")),
	}
	project := NewShellNixProjectFile(mustAbsPath(filepath.Join(dir, "shell.nix")))
	if err := os.WriteFile(nixFilePathForProject(project), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Both a new registration and an existing one tolerate an offline daemon.
	for i := 0; i < 2; i++ {
		if err := opWatch(paths, project); err != nil {
			t.Fatalf("opWatch call %d: %v", i+1, err)
		}
	}
	db, err := OpenLorriDB(paths.SQLiteDB.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registered, err := db.FindRegisteredProjectForDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if registered == nil || registered.NixFile != nixFilePathForProject(project) {
		t.Fatalf("project was not retained: %#v", registered)
	}
}

func TestWatchResumeCapability(t *testing.T) {
	out, err := exec.Command(lorriBinForTest(t), "internal", "capabilities_").Output()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"native-session-v2", "env-file-v1", "watch-resume-v1"}
	if got := strings.Fields(string(out)); !reflect.DeepEqual(got, want) {
		t.Fatalf("capabilities = %q, want %q", got, want)
	}
}
