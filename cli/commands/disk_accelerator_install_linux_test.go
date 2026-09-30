package commands

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"miren.dev/runtime/clientconfig"
	"miren.dev/runtime/pkg/labs"
	"miren.dev/runtime/pkg/runnerconfig"
)

func TestCurrentDiskAcceleratorNode(t *testing.T) {
	dir := t.TempDir()
	runnerPath := filepath.Join(dir, "runner.yaml")
	unitPath := filepath.Join(dir, "miren.service")
	serverPath := filepath.Join(dir, "server.toml")
	ctx := &Context{}

	_, _, _, err := currentDiskAcceleratorNode(ctx, runnerPath, unitPath, serverPath)
	require.ErrorContains(t, err, "pass a node name or ID")

	require.NoError(t, os.WriteFile(unitPath, nil, 0644))
	require.NoError(t, os.WriteFile(serverPath, []byte("[server]\nrunner_id = 'coordinator-99'\nconfig_cluster_name = 'this-host'\n"), 0644))
	id, runner, cluster, err := currentDiskAcceleratorNode(ctx, runnerPath, unitPath, serverPath)
	require.NoError(t, err)
	require.Equal(t, "coordinator-99", id)
	require.Nil(t, runner)
	require.Equal(t, "this-host", cluster)
	require.NoError(t, os.WriteFile(serverPath, []byte("[server]\n"), 0644))
	id, _, cluster, err = currentDiskAcceleratorNode(ctx, runnerPath, unitPath, serverPath)
	require.NoError(t, err)
	require.Equal(t, "miren", id)
	require.Equal(t, "local", cluster)
	t.Setenv("MIREN_SERVER_RUNNER_ID", "remote-env-node")
	t.Setenv("MIREN_SERVER_CONFIG_CLUSTER_NAME", "remote-env-cluster")
	id, _, cluster, err = currentDiskAcceleratorNode(ctx, runnerPath, unitPath, serverPath)
	require.NoError(t, err)
	require.Equal(t, "miren", id)
	require.Equal(t, "local", cluster)

	require.NoError(t, (&runnerconfig.Config{RunnerID: "runner-42"}).Save(runnerPath))
	id, runner, cluster, err = currentDiskAcceleratorNode(ctx, runnerPath, unitPath, serverPath)
	require.NoError(t, err)
	require.Equal(t, "runner-42", id)
	require.Equal(t, "runner-42", runner.RunnerID)
	require.Empty(t, cluster)

	require.NoError(t, os.WriteFile(runnerPath, []byte("runner_id: [invalid"), 0600))
	_, _, _, err = currentDiskAcceleratorNode(ctx, runnerPath, unitPath, serverPath)
	require.ErrorContains(t, err, "reading local runner identity")
}

func TestLocalDiskAcceleratorClusterIgnoresRemoteLocalAlias(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")
	t.Setenv("MIREN_CONFIG", filepath.Join(home, "remote.yaml"))
	t.Setenv("MIREN_SERVER_ADDRESS", "remote.example:8443")
	serverData := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(serverData, "server"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(serverData, "server", "ca.crt"), []byte("installed-ca"), 0644))
	serverConfig := filepath.Join(t.TempDir(), "server.toml")
	require.NoError(t, os.WriteFile(serverConfig, []byte("[server]\naddress = '0.0.0.0:9443'\ndata_path = '"+serverData+"'\n"), 0644))
	remote := clientconfig.NewConfig()
	remote.SetCluster("local", &clientconfig.ClusterConfig{Hostname: "remote.example:8443", CACert: "remote-ca"})
	require.NoError(t, remote.SaveTo(filepath.Join(home, "remote.yaml")))
	leafDir := filepath.Join(home, ".config/miren/clientconfig.d")
	require.NoError(t, os.MkdirAll(leafDir, 0700))
	leaf := "clusters:\n  local:\n    hostname: localhost:9555\n    ca_cert: installed-ca\n    client_cert: local-cert\n    client_key: local-key\n"
	require.NoError(t, os.WriteFile(filepath.Join(leafDir, "50-local.yaml"), []byte(leaf), 0600))
	ctx := &Context{}
	cluster, err := localDiskAcceleratorCluster(ctx, "local", serverConfig)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:9555", cluster.Hostname)
	require.Equal(t, "installed-ca", cluster.CACert)
	require.Equal(t, "local-cert", cluster.ClientCert)
	require.NoError(t, os.WriteFile(serverConfig, []byte("[server]\naddress = '0.0.0.0:9443'\ndata_path = '"+serverData+"'\nconfig_cluster_name = 'this-host'\n"), 0644))
	unit := filepath.Join(t.TempDir(), "miren.service")
	require.NoError(t, os.WriteFile(unit, nil, 0644))
	_, _, inferredName, err := currentDiskAcceleratorNode(ctx, filepath.Join(home, "no-runner.yaml"), unit, serverConfig)
	require.NoError(t, err)
	require.Equal(t, "this-host", inferredName)
	cluster, err = localDiskAcceleratorCluster(ctx, inferredName, serverConfig)
	require.NoError(t, err, "the installer writes a 'local' leaf even when the server has a custom cluster name")
	require.Equal(t, "127.0.0.1:9555", cluster.Hostname)

	require.NoError(t, os.WriteFile(filepath.Join(leafDir, "50-local.yaml"), []byte(strings.Replace(leaf, "installed-ca", "remote-ca", 1)), 0600))
	_, err = localDiskAcceleratorCluster(ctx, "local", serverConfig)
	require.ErrorContains(t, err, "do not match the installed server")

	require.NoError(t, os.WriteFile(serverConfig, []byte("[server]\naddress = 'remote.example:9443'\ndata_path = '"+serverData+"'\n"), 0644))
	_, err = localDiskAcceleratorCluster(ctx, "local", serverConfig)
	require.ErrorContains(t, err, "not loopback")
}

func TestDiskAcceleratorInstallDoesNotInferIntoSelectedCluster(t *testing.T) {
	labs.EnableAll()
	err := dispatchErr(t, "disk", "accelerator", "install", "--cluster", "remote")
	require.ErrorContains(t, err, "cannot infer this host's node")
	help := captureDispatch(t, []string{"disk", "accelerator", "install", "--help"})
	require.NotContains(t, help, "--best-attempt")
	require.NotContains(t, help, "--containerd-socket")
	require.Contains(t, help, "omit to infer this host's node")
}

func TestServerInstallOffersDiskAccelerator(t *testing.T) {
	out := captureDispatch(t, []string{"server", "install", "--help"})
	require.Contains(t, out, "--disk-accelerator")
	require.Contains(t, out, "before starting the server")
}

func TestFindBundledExecutableFallsBackFromCLIOnlyRelease(t *testing.T) {
	userRelease := t.TempDir()
	systemRelease := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(userRelease, "miren"), nil, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(systemRelease, "containerd"), nil, 0755))

	path, dir := findBundledExecutable("containerd", userRelease, systemRelease)
	require.Equal(t, filepath.Join(systemRelease, "containerd"), path)
	require.Equal(t, systemRelease, dir)
}

func TestUnixSocketListening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.sock")
	active, err := unixSocketListening(path)
	require.NoError(t, err)
	require.False(t, active)

	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	active, err = unixSocketListening(path)
	require.NoError(t, err)
	require.True(t, active)
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, listener.Close())
	active, err = unixSocketListening(path)
	require.NoError(t, err)
	require.False(t, active, "a stopped service may leave its socket inode behind")
}

func TestLocalDiskAcceleratorLockCoversRuntimeLifetime(t *testing.T) {
	dataPath := t.TempDir()
	unlock, err := lockLocalDiskAccelerator(dataPath)
	require.NoError(t, err)
	_, err = lockLocalDiskAccelerator(dataPath)
	require.ErrorContains(t, err, "another local accelerator installation is in progress")
	unlock()
	unlock, err = lockLocalDiskAccelerator(dataPath)
	require.NoError(t, err)
	unlock()
}

func TestBuildLocalLbdToolchainUsesEmbeddedContextAndLocalNamespace(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "buildctl"), nil, 0755))
	argsFile := filepath.Join(dir, "args")
	nerdctl := filepath.Join(dir, "nerdctl")
	require.NoError(t, os.WriteFile(nerdctl, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS_FILE\"\nprintf '%s\\n' \"$BUILDKIT_HOST\" > \"$BUILDKIT_FILE\"\ntest -f \"$8/Dockerfile\" && test -f \"$8/build.sh\"\n"), 0755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ARGS_FILE", argsFile)
	buildkitFile := filepath.Join(dir, "buildkit-host")
	t.Setenv("BUILDKIT_FILE", buildkitFile)

	ctx := &Context{Context: context.Background(), Stderr: io.Discard, Stdout: io.Discard}
	require.NoError(t, buildLocalLbdToolchain(ctx, "/run/example.sock", "/run/example-buildkit.sock", "localhost/lbd:test", dir))
	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	fields := strings.Split(strings.TrimSpace(string(args)), "\n")
	require.Equal(t, []string{"--address", "/run/example.sock", "--namespace", "miren", "build", "--tag", "localhost/lbd:test"}, fields[:7])
	buildkitHost, err := os.ReadFile(buildkitFile)
	require.NoError(t, err)
	require.Equal(t, "unix:///run/example-buildkit.sock\n", string(buildkitHost))
	_, err = os.Stat(fields[7])
	require.ErrorIs(t, err, os.ErrNotExist, "temporary build context must be removed")
}

func TestBuildLocalLbdToolchainFindsBundledBuildctl(t *testing.T) {
	home := t.TempDir()
	oldRelease := filepath.Join(home, ".miren", "release")
	require.NoError(t, os.MkdirAll(oldRelease, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(oldRelease, "nerdctl"), []byte("#!/bin/sh\nexit 9\n"), 0755))
	release := t.TempDir()
	require.NoError(t, os.MkdirAll(release, 0755))
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")
	t.Setenv("PATH", t.TempDir()) // buildctl is only in the release
	require.NoError(t, os.WriteFile(filepath.Join(release, "buildctl"), []byte("#!/bin/sh\nexit 0\n"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(release, "nerdctl"), []byte("#!/bin/sh\ncommand -v buildctl > \"$FOUND_BUILDCTL\"\n"), 0755))
	found := filepath.Join(t.TempDir(), "buildctl-path")
	t.Setenv("FOUND_BUILDCTL", found)
	ctx := &Context{Context: context.Background(), Stderr: io.Discard, Stdout: io.Discard}
	require.NoError(t, buildLocalLbdToolchain(ctx, "/run/example.sock", "/run/buildkit.sock", "localhost/lbd:test", release))
	path, err := os.ReadFile(found)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(release, "buildctl")+"\n", string(path))
}
