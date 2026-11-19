package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Both first cached entry and the same-hash prompt must send a conditional
// request, preserving the registered project kind. No Nix evaluation is needed.
func TestNativeCachedExportRebuildPolicy(t *testing.T) {
	for _, flake := range []bool{false, true} {
		name := "shell"
		if flake {
			name = "flake"
		}
		t.Run(name, func(t *testing.T) {
			socketDir, err := os.MkdirTemp("", "lorri-export-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(socketDir) })
			listener, lock, err := NewSocketPath(mustAbsPath(filepath.Join(socketDir, "daemon.socket"))).Bind()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close(); lock.Release() })
			type result struct {
				request PingRequest
				err     error
			}
			requests := make(chan result, 2)
			go func() {
				for i := 0; i < 2; i++ {
					request, err := func() (PingRequest, error) {
						var request PingRequest
						conn, err := listener.Accept()
						if err != nil {
							return request, err
						}
						framing := NewFraming(conn)
						defer framing.Close()
						var comm CommunicationType
						if err := framing.ReadMsg(defaultReadTimeout, &comm); err != nil {
							return request, err
						}
						if comm != CommPing {
							return request, fmt.Errorf("communication type = %q, want Ping", comm)
						}
						if err := framing.WriteMsg(defaultReadTimeout, ConnectionAccepted{}); err != nil {
							return request, err
						}
						err = framing.ReadMsg(defaultReadTimeout, &request)
						return request, err
					}()
					requests <- result{request, err}
					if err != nil {
						return
					}
				}
			}()
			var want ProjectFile
			tc := shellTestCases[0]
			tc.script = func(project, binary, home string) string {
				want = NewShellNixProjectFile(mustAbsPath(filepath.Join(project, "shell.nix")))
				if flake {
					want = NewFlakeProjectFile(mustAbsPath(project), "")
				}
				return fmt.Sprintf(`
set -eu
cd %q
export LORRI_SHELL_PID=$$
export LORRI_RUNTIME_DIR=%q
updates=$(%q export bash)
eval "$updates"
[[ $MARKER == hello ]]
updates=$(%q export bash)
[[ -z $updates ]]
`, project, socketDir, binary, binary)
			}
			runExportProject(t, tc, flake)
			for i := 0; i < 2; i++ {
				select {
				case got := <-requests:
					if got.err != nil {
						t.Fatal(got.err)
					}
					if got.request.Rebuild != RebuildOnlyIfNotYetWatching || !reflect.DeepEqual(got.request.ProjectFile, want) {
						t.Fatalf("cached export ping = %+v, want conditional ping for %+v", got.request, want)
					}
				case <-time.After(3 * time.Second):
					t.Fatalf("cached export %d did not request watcher resumption", i+1)
				}
			}
		})
	}
}
