// +build linux

package libcontainer

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/docker/libcontainer/apparmor"
	"github.com/docker/libcontainer/cgroups"
	"github.com/docker/libcontainer/configs"
	"github.com/docker/libcontainer/label"
	"github.com/docker/libcontainer/mount"
	"github.com/docker/libcontainer/network"
	"github.com/docker/libcontainer/system"
	"github.com/golang/glog"
)

const (
	EXIT_SIGNAL_OFFSET = 128
)

type initError struct {
	Message string `json:"message,omitempty"`
}

func (i initError) Error() string {
	return i.Message
}

type linuxContainer struct {
	id            string
	root          string
	config        *configs.Config
	state         *configs.State
	cgroupManager cgroups.Manager
	initArgs      []string
}

// ID returns the container's unique ID
func (c *linuxContainer) ID() string {
	return c.id
}

// Config returns the container's configuration
func (c *linuxContainer) Config() configs.Config {
	return *c.config
}

func (c *linuxContainer) Status() (configs.Status, error) {
	if c.state.InitPid <= 0 {
		return configs.Destroyed, nil
	}
	// return Running if the init process is alive
	err := syscall.Kill(c.state.InitPid, 0)
	if err != nil {
		if err == syscall.ESRCH {
			return configs.Destroyed, nil
		}
		return 0, err
	}
	if c.config.Cgroups != nil &&
		c.config.Cgroups.Freezer == configs.Frozen {
		return configs.Paused, nil
	}
	return configs.Running, nil
}

func (c *linuxContainer) Processes() ([]int, error) {
	glog.Info("fetch container processes")
	pids, err := c.cgroupManager.GetPids()
	if err != nil {
		return nil, newGenericError(err, SystemError)
	}
	return pids, nil
}

func (c *linuxContainer) Stats() (*Stats, error) {
	glog.Info("fetch container stats")
	var (
		err   error
		stats = &Stats{}
	)
	if stats.CgroupStats, err = c.cgroupManager.GetStats(); err != nil {
		return stats, newGenericError(err, SystemError)
	}
	if stats.NetworkStats, err = network.GetStats(&c.state.NetworkState); err != nil {
		return stats, newGenericError(err, SystemError)
	}
	return stats, nil
}

func (c *linuxContainer) Start(process *Process) (int, error) {
	status, err := c.Status()
	if err != nil {
		return -1, err
	}
	cmd := exec.Command(c.initArgs[0], c.initArgs[1:]...)
	cmd.Stdin = process.Stdin
	cmd.Stdout = process.Stdout
	cmd.Stderr = process.Stderr
	cmd.Env = c.config.Env
	cmd.Dir = c.config.RootFs
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// TODO: add pdeath to config for a container
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	if status != configs.Destroyed {
		glog.Info("start new container process")
		// TODO: (crosbymichael) check out console use for execin
		//return namespaces.ExecIn(process.Args, c.config.Env, "", cmd, c.config, c.state)
		return c.startNewProcess(cmd, process.Args)
	}
	if err := c.startInitProcess(cmd, process.Args); err != nil {
		return -1, err
	}
	return c.state.InitPid, nil
}

func (c *linuxContainer) startNewProcess(cmd *exec.Cmd, args []string) (int, error) {
	var err error
	parent, child, err := newInitPipe()
	if err != nil {
		return -1, err
	}
	defer parent.Close()
	cmd.ExtraFiles = []*os.File{child}
	cmd.Env = append(cmd.Env, fmt.Sprintf("_LIBCONTAINER_INITPID=%d", c.state.InitPid))
	if err := cmd.Start(); err != nil {
		child.Close()
		return -1, err
	}
	child.Close()
	s, err := cmd.Process.Wait()
	if err != nil {
		return -1, err
	}
	if !s.Success() {
		return -1, &exec.ExitError{s}
	}
	decoder := json.NewDecoder(parent)
	var pid *pid
	if err := decoder.Decode(&pid); err != nil {
		return -1, err
	}
	p, err := os.FindProcess(pid.Pid)
	if err != nil {
		return -1, err
	}
	terminate := func(terr error) (int, error) {
		// TODO: log the errors for kill and wait
		p.Kill()
		p.Wait()
		return -1, terr
	}
	// Enter cgroups.
	if err := enterCgroups(c.state, pid.Pid); err != nil {
		return terminate(err)
	}
	encoder := json.NewEncoder(parent)
	if err := encoder.Encode(c.config); err != nil {
		return terminate(err)
	}
	process := processArgs{
		Config: c.config,
		Args:   args,
	}
	if err := encoder.Encode(process); err != nil {
		return terminate(err)
	}
	return pid.Pid, nil
}

func (c *linuxContainer) startInitProcess(cmd *exec.Cmd, args []string) error {
	// create a pipe so that we can syncronize with the namespaced process and
	// pass the state and configuration to the child process
	parent, child, err := newInitPipe()
	if err != nil {
		return err
	}
	defer parent.Close()
	cmd.ExtraFiles = []*os.File{child}
	cmd.SysProcAttr.Cloneflags = c.config.Namespaces.CloneFlags()
	if c.config.Namespaces.Contains(configs.NEWUSER) {
		addUidGidMappings(cmd.SysProcAttr, c.config)
		// Default to root user when user namespaces are enabled.
		if cmd.SysProcAttr.Credential == nil {
			cmd.SysProcAttr.Credential = &syscall.Credential{}
		}
	}
	glog.Info("starting container init process")
	err = cmd.Start()
	child.Close()
	if err != nil {
		return newGenericError(err, SystemError)
	}
	wait := func() (*os.ProcessState, error) {
		ps, err := cmd.Process.Wait()
		// we should kill all processes in cgroup when init is died if we use
		// host PID namespace
		if !c.config.Namespaces.Contains(configs.NEWPID) {
			c.killAllPids()
		}
		return ps, newGenericError(err, SystemError)
	}
	terminate := func(terr error) error {
		// TODO: log the errors for kill and wait
		cmd.Process.Kill()
		wait()
		return terr
	}
	started, err := system.GetProcessStartTime(cmd.Process.Pid)
	if err != nil {
		return terminate(err)
	}
	// Do this before syncing with child so that no children
	// can escape the cgroup
	if err := c.cgroupManager.Apply(cmd.Process.Pid); err != nil {
		return terminate(err)
	}
	defer func() {
		if err != nil {
			c.cgroupManager.Destroy()
		}
	}()
	var networkState configs.NetworkState
	if err := c.initializeNetworking(cmd.Process.Pid, &networkState); err != nil {
		return terminate(err)
	}
	process := processArgs{
		Args:         args,
		Config:       c.config,
		NetworkState: &networkState,
	}
	// Start the setup process to setup the init process
	if c.config.Namespaces.Contains(configs.NEWUSER) {
		if err = executeSetupCmd(cmd.Args, cmd.Process.Pid, c.config, &process, &networkState); err != nil {
			return terminate(err)
		}
	}
	// send the state to the container's init process then shutdown writes for the parent
	if err := json.NewEncoder(parent).Encode(process); err != nil {
		return terminate(err)
	}
	// shutdown writes for the parent side of the pipe
	if err := syscall.Shutdown(int(parent.Fd()), syscall.SHUT_WR); err != nil {
		return terminate(err)
	}
	// wait for the child process to fully complete and receive an error message
	// if one was encoutered
	var ierr *initError
	if err := json.NewDecoder(parent).Decode(&ierr); err != nil && err != io.EOF {
		return terminate(err)
	}
	if ierr != nil {
		return terminate(ierr)
	}

	c.state.InitPid = cmd.Process.Pid
	c.state.InitStartTime = started
	c.state.NetworkState = networkState
	c.state.CgroupPaths = c.cgroupManager.GetPaths()

	return nil
}

func (c *linuxContainer) Destroy() error {
	status, err := c.Status()
	if err != nil {
		return err
	}
	if status != configs.Destroyed {
		return newGenericError(nil, ContainerNotStopped)
	}
	return os.RemoveAll(c.root)
}

func (c *linuxContainer) Pause() error {
	return c.cgroupManager.Freeze(configs.Frozen)
}

func (c *linuxContainer) Resume() error {
	return c.cgroupManager.Freeze(configs.Thawed)
}

func (c *linuxContainer) Signal(signal os.Signal) error {
	glog.Infof("sending signal %d to pid %d", signal, c.state.InitPid)
	panic("not implemented")
}

func (c *linuxContainer) OOM() (<-chan struct{}, error) {
	return NotifyOnOOM(c.state)
}

func (c *linuxContainer) updateStateFile() error {
	fnew := filepath.Join(c.root, fmt.Sprintf("%s.new", stateFilename))
	f, err := os.Create(fnew)
	if err != nil {
		return newGenericError(err, SystemError)
	}
	defer f.Close()

	if err := json.NewEncoder(f).Encode(c.state); err != nil {
		f.Close()
		os.Remove(fnew)
		return newGenericError(err, SystemError)
	}
	fname := filepath.Join(c.root, stateFilename)
	if err := os.Rename(fnew, fname); err != nil {
		return newGenericError(err, SystemError)
	}
	return nil
}

// New returns a newly initialized Pipe for communication between processes
func newInitPipe() (parent *os.File, child *os.File, err error) {
	fds, err := syscall.Socketpair(syscall.AF_LOCAL, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[1]), "parent"), os.NewFile(uintptr(fds[0]), "child"), nil
}

// Converts IDMap to SysProcIDMap array and adds it to SysProcAttr.
func addUidGidMappings(sys *syscall.SysProcAttr, container *configs.Config) {
	if container.UidMappings != nil {
		sys.UidMappings = make([]syscall.SysProcIDMap, len(container.UidMappings))
		for i, um := range container.UidMappings {
			sys.UidMappings[i].ContainerID = um.ContainerID
			sys.UidMappings[i].HostID = um.HostID
			sys.UidMappings[i].Size = um.Size
		}
	}

	if container.GidMappings != nil {
		sys.GidMappings = make([]syscall.SysProcIDMap, len(container.GidMappings))
		for i, gm := range container.GidMappings {
			sys.GidMappings[i].ContainerID = gm.ContainerID
			sys.GidMappings[i].HostID = gm.HostID
			sys.GidMappings[i].Size = gm.Size
		}
	}
}

// killAllPids iterates over all of the container's processes
// sending a SIGKILL to each process.
func (c *linuxContainer) killAllPids() error {
	glog.Info("killing all processes in container")
	var procs []*os.Process
	c.cgroupManager.Freeze(configs.Frozen)
	pids, err := c.cgroupManager.GetPids()
	if err != nil {
		return err
	}
	for _, pid := range pids {
		// TODO: log err without aborting if we are unable to find
		// a single PID
		if p, err := os.FindProcess(pid); err == nil {
			procs = append(procs, p)
			p.Kill()
		}
	}
	c.cgroupManager.Freeze(configs.Thawed)
	for _, p := range procs {
		p.Wait()
	}
	return err
}

// initializeNetworking creates the container's network stack outside of the namespace and moves
// interfaces into the container's net namespaces if necessary
func (c *linuxContainer) initializeNetworking(nspid int, networkState *configs.NetworkState) error {
	glog.Info("initailzing container's network stack")
	for _, config := range c.config.Networks {
		strategy, err := network.GetStrategy(config.Type)
		if err != nil {
			return err
		}
		if err := strategy.Create(config, nspid, networkState); err != nil {
			return err
		}
	}
	return nil
}

func executeSetupCmd(args []string, ppid int, container *configs.Config, process *processArgs, networkState *configs.NetworkState) error {
	command := exec.Command(args[0], args[1:]...)
	parent, child, err := newInitPipe()
	if err != nil {
		return err
	}
	defer parent.Close()
	command.ExtraFiles = []*os.File{child}
	command.Dir = container.RootFs
	command.Env = append(command.Env,
		fmt.Sprintf("_LIBCONTAINER_INITPID=%d", ppid),
		fmt.Sprintf("_LIBCONTAINER_USERNS=1"))
	err = command.Start()
	child.Close()
	if err != nil {
		return err
	}
	s, err := command.Process.Wait()
	if err != nil {
		return err
	}
	if !s.Success() {
		return &exec.ExitError{s}
	}
	decoder := json.NewDecoder(parent)
	var pid *pid
	if err := decoder.Decode(&pid); err != nil {
		return err
	}
	p, err := os.FindProcess(pid.Pid)
	if err != nil {
		return err
	}
	terminate := func(terr error) error {
		// TODO: log the errors for kill and wait
		p.Kill()
		p.Wait()
		return terr
	}
	// send the state to the container's init process then shutdown writes for the parent
	if err := json.NewEncoder(parent).Encode(process); err != nil {
		return terminate(err)
	}
	// shutdown writes for the parent side of the pipe
	if err := syscall.Shutdown(int(parent.Fd()), syscall.SHUT_WR); err != nil {
		return terminate(err)
	}
	// wait for the child process to fully complete and receive an error message
	// if one was encoutered
	var ierr *initError
	if err := decoder.Decode(&ierr); err != nil && err != io.EOF {
		return terminate(err)
	}
	if ierr != nil {
		return ierr
	}
	s, err = p.Wait()
	if err != nil {
		return err
	}
	if !s.Success() {
		return &exec.ExitError{s}
	}
	return nil
}

type pid struct {
	Pid int `json:"Pid"`
}

// Finalize entering into a container and execute a specified command
func InitIn(pipe *os.File) (err error) {
	defer func() {
		// if we have an error during the initialization of the container's init then send it back to the
		// parent process in the form of an initError.
		if err != nil {
			// ensure that any data sent from the parent is consumed so it doesn't
			// receive ECONNRESET when the child writes to the pipe.
			ioutil.ReadAll(pipe)
			if err := json.NewEncoder(pipe).Encode(initError{
				Message: err.Error(),
			}); err != nil {
				panic(err)
			}
		}
		// ensure that this pipe is always closed
		pipe.Close()
	}()
	decoder := json.NewDecoder(pipe)
	var config *configs.Config
	if err := decoder.Decode(&config); err != nil {
		return err
	}
	var process *processArgs
	if err := decoder.Decode(&process); err != nil {
		return err
	}
	if err := finalizeSetns(config); err != nil {
		return err
	}
	if err := system.Execv(process.Args[0], process.Args[0:], config.Env); err != nil {
		return err
	}
	panic("unreachable")
}

// finalize expects that the setns calls have been setup and that is has joined an
// existing namespace
func finalizeSetns(container *configs.Config) error {
	// clear the current processes env and replace it with the environment defined on the container
	if err := loadContainerEnvironment(container); err != nil {
		return err
	}

	if err := setupRlimits(container); err != nil {
		return fmt.Errorf("setup rlimits %s", err)
	}

	if err := finalizeNamespace(container); err != nil {
		return err
	}

	if err := apparmor.ApplyProfile(container.AppArmorProfile); err != nil {
		return fmt.Errorf("set apparmor profile %s: %s", container.AppArmorProfile, err)
	}

	if container.ProcessLabel != "" {
		if err := label.SetProcessLabel(container.ProcessLabel); err != nil {
			return err
		}
	}

	return nil
}

// SetupContainer is run to setup mounts and networking related operations
// for a user namespace enabled process as a user namespace root doesn't
// have permissions to perform these operations.
// The setup process joins all the namespaces of user namespace enabled init
// except the user namespace, so it run as root in the root user namespace
// to perform these operations.
func SetupContainer(process *processArgs) error {
	container := process.Config
	networkState := process.NetworkState

	// TODO : move to validation
	/*
		rootfs, err := utils.ResolveRootfs(container.RootFs)
		if err != nil {
			return err
		}
	*/

	// clear the current processes env and replace it with the environment
	// defined on the container
	if err := loadContainerEnvironment(container); err != nil {
		return err
	}

	cloneFlags := container.Namespaces.CloneFlags()
	if (cloneFlags & syscall.CLONE_NEWNET) == 0 {
		if len(container.Networks) != 0 || len(container.Routes) != 0 {
			return fmt.Errorf("unable to apply network parameters without network namespace")
		}
	} else {
		if err := setupNetwork(container, networkState); err != nil {
			return fmt.Errorf("setup networking %s", err)
		}
		if err := setupRoute(container); err != nil {
			return fmt.Errorf("setup route %s", err)
		}
	}

	label.Init()

	// InitializeMountNamespace() can be executed only for a new mount namespace
	if (cloneFlags & syscall.CLONE_NEWNS) != 0 {
		if err := mount.InitializeMountNamespace(container); err != nil {
			return fmt.Errorf("setup mount namespace %s", err)
		}
	}
	return nil
}

func enterCgroups(state *configs.State, pid int) error {
	return cgroups.EnterPid(state.CgroupPaths, pid)
}
