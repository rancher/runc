package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/codegangsta/cli"
	"github.com/docker/docker/pkg/mount"
	"github.com/docker/docker/pkg/term"
	"github.com/opencontainers/runc/libcontainer"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/syndtr/gocapability/capability"
)

const (
	version                   = "0.0.0"
	specConfig                = "config.json"
	runtimeConfig             = "runtime.json"
	driverRunRunc             = "/run/runc"
	driverVarRunRunc          = "/var/run/runc"
	driverRun                 = "/var/run/docker/execdriver/native"
	containerDriverRun        = "/host" + driverRun
	containerDriverRunRunc    = "/host" + driverRunRunc
	containerDriverVarRunRunc = "/host" + driverVarRunRunc
	libcontainerRoot          = "/var/run/rancher/container"
)

var (
	cgroupPattern = regexp.MustCompile("^.*/docker-([a-z0-9]+).scope$")
)

func main() {
	app := cli.NewApp()
	app.Version = version
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name: "stage2",
		},
	}
	app.Action = func(cli *cli.Context) {
		var fun cliFunc

		if cli.GlobalBool("stage2") {
			fun = stage2
		} else {
			fun = start
		}

		i, err := fun(cli)
		if err != nil {
			logrus.Fatal(err)
		}
		os.Exit(i)
	}
	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

type cliFunc func(cli *cli.Context) (int, error)

func allCaps() []string {
	caps := []string{}
	for _, cap := range capability.List() {
		if cap > capability.CAP_LAST_CAP {
			continue
		}
		caps = append(caps, fmt.Sprintf("CAP_%s", strings.ToUpper(cap.String())))
	}

	return caps
}

func stage2(cli *cli.Context) (int, error) {
	args := []string{}

	state, err := findState(driverRunRunc, driverVarRunRunc, driverRun)
	if err != nil {
		return -1, err
	}

	var config configs.Config
	config.RootPropagation = syscall.MS_SHARED
	config.Devices = state.Config.Devices
	config.Rootfs = state.Config.Rootfs
	config.Mounts = state.Config.Mounts
	config.Capabilities = allCaps()
	config.Namespaces = configs.Namespaces{
		configs.Namespace{
			Type: configs.NEWNS,
		},
	}

	for i, val := range cli.Args() {
		if val == "--" {
			args = cli.Args()[i+1:]
			break
		}

		if _, err := os.Stat(val); os.IsNotExist(err) {
			if err := os.MkdirAll(val, 0755); err != nil {
				return -1, err
			}
		}

		if err := mount.MakeShared(val); err != nil {
			logrus.Errorf("Failed to make shared %s: %v", val, err)
			return -1, err
		}

		config.Mounts = append(config.Mounts, &configs.Mount{
			Source:      val,
			Destination: val,
			Device:      "bind",
			// I don't actually know what 20480 is...
			Flags:            20480,
			PropagationFlags: []int{syscall.MS_SHARED},
		})
	}

	return run(&config, randomString(12), args)
}

func start(cli *cli.Context) (int, error) {
	state, err := findState(containerDriverRunRunc, containerDriverVarRunRunc, containerDriverRun)
	if err != nil {
		return -1, err
	}

	mnt, err := getMntFd(state.InitProcessPid)
	if err != nil {
		return -1, err
	}

	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		return -1, err
	}

	nsenter, err := exec.LookPath("nsenter")
	if err != nil {
		logrus.Error("Failed to find nsenter:", err)
		return -1, err
	}

	args := []string{nsenter, "--mount=" + mnt, "-F", "--", path.Join(state.Config.Rootfs, self), "--stage2"}
	args = append(args, os.Args[1:]...)

	logrus.Infof("Execing %v", args)
	return -1, syscall.Exec(nsenter, args, os.Environ())
}

func getMntFd(pid int) (string, error) {
	psStat := fmt.Sprintf("/proc/%d/stat", pid)
	content, err := ioutil.ReadFile(psStat)
	if err != nil {
		return "", err
	}

	ppid := strings.Split(strings.SplitN(string(content), ")", 2)[1], " ")[2]
	return fmt.Sprintf("/proc/%s/ns/mnt", ppid), nil
}

func findContainerId() (string, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/cgroup", os.Getpid()))
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "docker/") && strings.Contains(line, ":devices:") {
			parts := strings.Split(line, "/")
			return parts[len(parts)-1], nil
		}
	}

	f.Seek(0, 0)
	scanner = bufio.NewScanner(f)
	for scanner.Scan() {
		matches := cgroupPattern.FindAllStringSubmatch(scanner.Text(), -1)
		if len(matches) > 0 && len(matches[0]) > 1 && matches[0][1] != "" {
			return matches[0][1], nil
		}
	}

	content, _ := ioutil.ReadFile(fmt.Sprintf("/proc/%d/cgroup", os.Getpid()))
	return "", fmt.Errorf("Failed to find container id:\n%s", string(content))
}

func findState(stateRoots ...string) (*libcontainer.State, error) {
	containerId, err := findContainerId()
	if err != nil {
		return nil, err
	}

	for _, stateRoot := range stateRoots {
		files, err := ioutil.ReadDir(stateRoot)
		if err != nil {
			continue
		}

		for _, file := range files {
			if !strings.HasPrefix(file.Name(), containerId) {
				continue
			}

			bytes, err := ioutil.ReadFile(path.Join(stateRoot, file.Name(), "state.json"))
			if err != nil {
				continue
			}

			var state libcontainer.State
			return &state, json.Unmarshal(bytes, &state)
		}
	}

	return nil, errors.New("Failed to find state.json")
}

func run(config *configs.Config, id string, args []string) (int, error) {
	if _, err := os.Stat(config.Rootfs); err != nil {
		if os.IsNotExist(err) {
			return -1, fmt.Errorf("Rootfs (%q) does not exist", config.Rootfs)
		}
		return -1, err
	}
	rootuid, err := config.HostUID()
	if err != nil {
		return -1, err
	}
	factory, err := libcontainer.New(libcontainerRoot, libcontainer.Cgroupfs, func(l *libcontainer.LinuxFactory) error {
		return nil
	})
	if err != nil {
		return -1, err
	}
	container, err := factory.Create(id, config)
	if err != nil {
		logrus.Errorf("Failed to create container %s: %v", id, err)
		return -1, err
	}

	_, isterm := term.GetFdInfo(os.Stdin)
	defer destroy(container)
	process := &libcontainer.Process{
		Args:   args,
		Env:    os.Environ(),
		User:   "0:0",
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	tty, err := newTty(isterm, process, rootuid)
	if err != nil {
		logrus.Errorf("Failed to create tty %s: %v", id, err)
		return -1, err
	}
	handler := newSignalHandler(tty)
	defer handler.Close()
	if err := container.Start(process); err != nil {
		logrus.Errorf("Failed to start (pid %d) %#v: %v", os.Getpid(), process, err)
		return -1, err
	}
	return handler.forward(process)
}

func randomString(strlen int) string {
	rand.Seed(time.Now().UTC().UnixNano())
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	result := make([]byte, strlen)
	for i := 0; i < strlen; i++ {
		result[i] = chars[rand.Intn(len(chars))]
	}
	return string(result)
}
