// +build linux

package libcontainer

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/mrunalp/fileutils"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/mount"
	"github.com/opencontainers/runc/libcontainer/system"
	"github.com/opencontainers/runc/libcontainer/utils"
	"github.com/opencontainers/selinux/go-selinux/label"
	"golang.org/x/sys/unix"
)

const defaultMountFlags = syscall.MS_NOEXEC | syscall.MS_NOSUID | syscall.MS_NODEV

// needsSetupDev returns true if /dev needs to be set up.
func needsSetupDev(config *configs.Config) bool {
	for _, m := range config.Mounts {
		if m.Device == "bind" && utils.CleanPath(m.Destination) == "/dev" {
			return false
		}
	}
	return true
}

// setupRootfs sets up the devices, mount points, and filesystems for use inside a
// new mount namespace.
func setupRootfs(config *configs.Config, console *linuxConsole, pipe io.ReadWriter) (err error) {
	if err := prepareRoot(config); err != nil {
		return newSystemErrorWithCause(err, "preparing rootfs")
	}

	setupDev := needsSetupDev(config)
	for _, m := range config.Mounts {
		for _, precmd := range m.PremountCmds {
			if err := mountCmd(precmd); err != nil {
				return newSystemErrorWithCause(err, "running premount command")
			}
		}
		if err := mountToRootfs(m, config.Rootfs, config.MountLabel); err != nil {
			return newSystemErrorWithCausef(err, "mounting %q to rootfs %q at %q", m.Source, config.Rootfs, m.Destination)
		}

		for _, postcmd := range m.PostmountCmds {
			if err := mountCmd(postcmd); err != nil {
				return newSystemErrorWithCause(err, "running postmount command")
			}
		}
	}
	if setupDev {
		if err := createDevices(config); err != nil {
			return newSystemErrorWithCause(err, "creating device nodes")
		}
		if err := setupPtmx(config, console); err != nil {
			return newSystemErrorWithCause(err, "setting up ptmx")
		}
		if err := setupDevSymlinks(config.Rootfs); err != nil {
			return newSystemErrorWithCause(err, "setting up /dev symlinks")
		}
	}
	// Signal the parent to run the pre-start hooks.
	// The hooks are run after the mounts are setup, but before we switch to the new
	// root, so that the old root is still available in the hooks for any mount
	// manipulations.
	if err := syncParentHooks(pipe); err != nil {
		return err
	}
	if err := syscall.Chdir(config.Rootfs); err != nil {
		return newSystemErrorWithCausef(err, "changing dir to %q", config.Rootfs)
	}
	if config.NoPivotRoot {
		err = msMoveRoot(config.Rootfs)
	} else {
		err = pivotRoot(config.Rootfs, config.PivotDir)
	}
	if err != nil {
		return newSystemErrorWithCause(err, "jailing process inside rootfs")
	}
	if setupDev {
		if err := reOpenDevNull(); err != nil {
			return newSystemErrorWithCause(err, "reopening /dev/null inside container")
		}
	}
	// remount dev as ro if specifed
	for _, m := range config.Mounts {
		if utils.CleanPath(m.Destination) == "/dev" {
			if m.Flags&unix.MS_RDONLY == unix.MS_RDONLY {
				if err := remountReadonly(m); err != nil {
					return newSystemErrorWithCausef(err, "remounting %q as readonly", m.Destination)
				}
			}
			break
		}
	}
	// set rootfs ( / ) as readonly
	if config.Readonlyfs {
		if err := setReadonly(); err != nil {
			return newSystemErrorWithCause(err, "setting rootfs as readonly")
		}
	}
	syscall.Umask(0022)
	return nil
}

func mountCmd(cmd configs.Command) error {
	command := exec.Command(cmd.Path, cmd.Args[:]...)
	command.Env = cmd.Env
	command.Dir = cmd.Dir
	if out, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("%#v failed: %s: %v", cmd, string(out), err)
	}
	return nil
}

// /tmp has to be mounted as private to allow MS_MOVE to work in all situations
func prepareTmp(topTmpDir string) (string, error) {
	tmpdir, err := ioutil.TempDir(topTmpDir, "runctop")
	if err != nil {
		return "", err
	}
	if err := unix.Mount(tmpdir, tmpdir, "bind", unix.MS_BIND, ""); err != nil {
		return "", err
	}
	if err := unix.Mount("", tmpdir, "", uintptr(unix.MS_PRIVATE), ""); err != nil {
		return "", err
	}
	return tmpdir, nil
}

func cleanupTmp(tmpdir string) {
	_ = unix.Unmount(tmpdir, 0)
	_ = os.RemoveAll(tmpdir)
}

func doTmpfsCopyUp(m *configs.Mount, rootfs, mountLabel string) (Err error) {
	// Set up a scratch dir for the tmpfs on the host.
	tmpdir, err := prepareTmp("/tmp")
	if err != nil {
		return newSystemErrorWithCause(err, "tmpcopyup: failed to setup tmpdir")
	}
	defer cleanupTmp(tmpdir)
	tmpDir, err := ioutil.TempDir(tmpdir, "runctmpdir")
	if err != nil {
		return newSystemErrorWithCause(err, "tmpcopyup: failed to create tmpdir")
	}
	defer os.RemoveAll(tmpDir)

	// Configure the *host* tmpdir as if it's the container mount. We change
	// m.Destination since we are going to mount *on the host*.
	oldDest := m.Destination
	m.Destination = tmpDir
	err = mountPropagate(m, "/", mountLabel)
	m.Destination = oldDest
	if err != nil {
		return err
	}
	defer func() {
		if Err != nil {
			if err := unix.Unmount(tmpDir, unix.MNT_DETACH); err != nil {
				logrus.Warnf("tmpcopyup: failed to unmount tmpdir on error: %v", err)
			}
		}
	}()

	return utils.WithProcfd(rootfs, m.Destination, func(procfd string) (Err error) {
		// Copy the container data to the host tmpdir. We append "/" to force
		// CopyDirectory to resolve the symlink rather than trying to copy the
		// symlink itself.
		if err := fileutils.CopyDirectory(procfd+"/", tmpDir); err != nil {
			return fmt.Errorf("tmpcopyup: failed to copy %s to %s (%s): %w", m.Destination, procfd, tmpDir, err)
		}
		// Now move the mount into the container.
		if err := unix.Mount(tmpDir, procfd, "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("tmpcopyup: failed to move mount %s to %s (%s): %w", tmpDir, procfd, m.Destination, err)
		}
		return nil
	})
}

func mountToRootfs(m *configs.Mount, rootfs, mountLabel string) error {
	dest, err := securejoin.SecureJoin(rootfs, m.Destination)
	if err != nil {
		return err
	}

	switch m.Device {
	case "proc", "sysfs":
		if err := os.MkdirAll(dest, 0755); err != nil {
			return err
		}
		// Selinux kernels do not support labeling of /proc or /sys
		return mountPropagate(m, rootfs, "")
	case "mqueue":
		if err := os.MkdirAll(dest, 0755); err != nil {
			return err
		}
		if err := mountPropagate(m, rootfs, mountLabel); err != nil {
			// older kernels do not support labeling of /dev/mqueue
			if err := mountPropagate(m, rootfs, ""); err != nil {
				return err
			}
			return label.SetFileLabel(dest, mountLabel)
		}
		return nil
	case "tmpfs":
		stat, err := os.Stat(dest)
		if err != nil {
			if err := os.MkdirAll(dest, 0755); err != nil {
				return err
			}
		}
		if m.Extensions&configs.EXT_COPYUP == configs.EXT_COPYUP {
			err = doTmpfsCopyUp(m, rootfs, mountLabel)
		} else {
			err = mountPropagate(m, rootfs, mountLabel)
		}
		if err != nil {
			return err
		}
		if stat != nil {
			if err = os.Chmod(dest, stat.Mode()); err != nil {
				return err
			}
		}
		return nil
	case "bind":
		stat, err := os.Stat(m.Source)
		if err != nil {
			// error out if the source of a bind mount does not exist as we will be
			// unable to bind anything to it.
			return err
		}
		// ensure that the destination of the bind mount is resolved of symlinks at mount time because
		// any previous mounts can invalidate the next mount's destination.
		// this can happen when a user specifies mounts within other mounts to cause breakouts or other
		// evil stuff to try to escape the container's rootfs.
		if dest, err = securejoin.SecureJoin(rootfs, m.Destination); err != nil {
			return err
		}
		if err := checkMountDestination(rootfs, dest); err != nil {
			return err
		}
		// update the mount with the correct dest after symlinks are resolved.
		m.Destination = dest
		if err := createIfNotExists(dest, stat.IsDir()); err != nil {
			return err
		}
		if err := mountPropagate(m, rootfs, mountLabel); err != nil {
			return err
		}
		// bind mount won't change mount options, we need remount to make mount options effective.
		// first check that we have non-default options required before attempting a remount
		if m.Flags&^(unix.MS_REC|unix.MS_REMOUNT|unix.MS_BIND) != 0 {
			// only remount if unique mount options are set
			if err := remount(m, rootfs); err != nil {
				return err
			}
		}

		if m.Relabel != "" {
			if err := label.Validate(m.Relabel); err != nil {
				return err
			}
			shared := label.IsShared(m.Relabel)
			if err := label.Relabel(m.Source, mountLabel, shared); err != nil {
				return err
			}
		}
	case "cgroup":
		binds, err := getCgroupMounts(m)
		if err != nil {
			return err
		}
		var merged []string
		for _, b := range binds {
			ss := filepath.Base(b.Destination)
			if strings.Contains(ss, ",") {
				merged = append(merged, ss)
			}
		}
		tmpfs := &configs.Mount{
			Source:           "tmpfs",
			Device:           "tmpfs",
			Destination:      m.Destination,
			Flags:            defaultMountFlags,
			Data:             "mode=755",
			PropagationFlags: m.PropagationFlags,
		}
		if err := mountToRootfs(tmpfs, rootfs, mountLabel); err != nil {
			return err
		}
		for _, b := range binds {
			if err := mountToRootfs(b, rootfs, mountLabel); err != nil {
				return err
			}
		}
		for _, mc := range merged {
			for _, ss := range strings.Split(mc, ",") {
				// symlink(2) is very dumb, it will just shove the path into
				// the link and doesn't do any checks or relative path
				// conversion. Also, don't error out if the cgroup already exists.
				if err := os.Symlink(mc, filepath.Join(rootfs, m.Destination, ss)); err != nil && !os.IsExist(err) {
					return err
				}
			}
		}
		if m.Flags&syscall.MS_RDONLY != 0 {
			// remount cgroup root as readonly
			mcgrouproot := &configs.Mount{
				Source:      m.Destination,
				Device:      "bind",
				Destination: m.Destination,
				Flags:       defaultMountFlags | syscall.MS_RDONLY | syscall.MS_BIND,
			}
			if err := remount(mcgrouproot, rootfs); err != nil {
				return err
			}
		}
	default:
		if err := checkMountDestination(rootfs, dest); err != nil {
			return err
		}
		if err := os.MkdirAll(dest, 0755); err != nil {
			return err
		}
		return mountPropagate(m, rootfs, mountLabel)
	}
	return nil
}

func getCgroupMounts(m *configs.Mount) ([]*configs.Mount, error) {
	mounts, err := cgroups.GetCgroupMounts(false)
	if err != nil {
		return nil, err
	}

	cgroupPaths, err := cgroups.ParseCgroupFile("/proc/self/cgroup")
	if err != nil {
		return nil, err
	}

	var binds []*configs.Mount

	for _, mm := range mounts {
		dir, err := mm.GetThisCgroupDir(cgroupPaths)
		if err != nil {
			return nil, err
		}
		relDir, err := filepath.Rel(mm.Root, dir)
		if err != nil {
			return nil, err
		}
		binds = append(binds, &configs.Mount{
			Device:           "bind",
			Source:           filepath.Join(mm.Mountpoint, relDir),
			Destination:      filepath.Join(m.Destination, strings.Join(mm.Subsystems, ",")),
			Flags:            syscall.MS_BIND | syscall.MS_REC | m.Flags,
			PropagationFlags: m.PropagationFlags,
		})
	}

	return binds, nil
}

// checkMountDestination checks to ensure that the mount destination is not over the top of /proc.
// dest is required to be an abs path and have any symlinks resolved before calling this function.
func checkMountDestination(rootfs, dest string) error {
	if utils.CleanPath(rootfs) == utils.CleanPath(dest) {
		return fmt.Errorf("mounting into / is prohibited")
	}
	invalidDestinations := []string{
		"/proc",
	}
	// White list, it should be sub directories of invalid destinations
	validDestinations := []string{
		// These entries can be bind mounted by files emulated by fuse,
		// so commands like top, free displays stats in container.
		"/proc/cpuinfo",
		"/proc/diskstats",
		"/proc/meminfo",
		"/proc/stat",
		"/proc/swaps",
		"/proc/uptime",
		"/proc/net/dev",
	}
	for _, valid := range validDestinations {
		path, err := filepath.Rel(filepath.Join(rootfs, valid), dest)
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
	}
	for _, invalid := range invalidDestinations {
		path, err := filepath.Rel(filepath.Join(rootfs, invalid), dest)
		if err != nil {
			return err
		}
		if path == "." || !strings.HasPrefix(path, "..") {
			return fmt.Errorf("%q cannot be mounted because it is located inside %q", dest, invalid)
		}
	}
	return nil
}

func setupDevSymlinks(rootfs string) error {
	var links = [][2]string{
		{"/proc/self/fd", "/dev/fd"},
		{"/proc/self/fd/0", "/dev/stdin"},
		{"/proc/self/fd/1", "/dev/stdout"},
		{"/proc/self/fd/2", "/dev/stderr"},
	}
	// kcore support can be toggled with CONFIG_PROC_KCORE; only create a symlink
	// in /dev if it exists in /proc.
	if _, err := os.Stat("/proc/kcore"); err == nil {
		links = append(links, [2]string{"/proc/kcore", "/dev/core"})
	}
	for _, link := range links {
		var (
			src = link[0]
			dst = filepath.Join(rootfs, link[1])
		)
		if err := os.Symlink(src, dst); err != nil && !os.IsExist(err) {
			return fmt.Errorf("symlink %s %s %s", src, dst, err)
		}
	}
	return nil
}

// If stdin, stdout, and/or stderr are pointing to `/dev/null` in the parent's rootfs
// this method will make them point to `/dev/null` in this container's rootfs.  This
// needs to be called after we chroot/pivot into the container's rootfs so that any
// symlinks are resolved locally.
func reOpenDevNull() error {
	var stat, devNullStat syscall.Stat_t
	file, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("Failed to open /dev/null - %s", err)
	}
	defer file.Close()
	if err := syscall.Fstat(int(file.Fd()), &devNullStat); err != nil {
		return err
	}
	for fd := 0; fd < 3; fd++ {
		if err := syscall.Fstat(fd, &stat); err != nil {
			return err
		}
		if stat.Rdev == devNullStat.Rdev {
			// Close and re-open the fd.
			if err := syscall.Dup3(int(file.Fd()), fd, 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// Create the device nodes in the container.
func createDevices(config *configs.Config) error {
	useBindMount := system.RunningInUserNS() || config.Namespaces.Contains(configs.NEWUSER)
	oldMask := syscall.Umask(0000)
	for _, node := range config.Devices {
		// containers running in a user namespace are not allowed to mknod
		// devices so we can just bind mount it from the host.
		if err := createDeviceNode(config.Rootfs, node, useBindMount); err != nil {
			syscall.Umask(oldMask)
			return err
		}
	}
	syscall.Umask(oldMask)
	return nil
}

func bindMountDeviceNode(rootfs, dest string, node *configs.Device) error {
	f, err := os.Create(dest)
	if err != nil && !os.IsExist(err) {
		return err
	}
	if f != nil {
		f.Close()
	}
	return utils.WithProcfd(rootfs, dest, func(procfd string) error {
		return unix.Mount(node.Path, procfd, "bind", unix.MS_BIND, "")
	})
}

// Creates the device node in the rootfs of the container.
func createDeviceNode(rootfs string, node *configs.Device, bind bool) error {
	if node.Path == "" {
		// The node only exists for cgroup reasons, ignore it here.
		return nil
	}
	dest, err := securejoin.SecureJoin(rootfs, node.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}

	if bind {
		return bindMountDeviceNode(rootfs, dest, node)
	}
	if err := mknodDevice(dest, node); err != nil {
		if os.IsExist(err) {
			return nil
		} else if os.IsPermission(err) {
			return bindMountDeviceNode(rootfs, dest, node)
		}
		return err
	}
	return nil
}

func mknodDevice(dest string, node *configs.Device) error {
	fileMode := node.FileMode
	switch node.Type {
	case 'c':
		fileMode |= syscall.S_IFCHR
	case 'b':
		fileMode |= syscall.S_IFBLK
	default:
		return fmt.Errorf("%c is not a valid device type for device %s", node.Type, node.Path)
	}
	if err := syscall.Mknod(dest, uint32(fileMode), node.Mkdev()); err != nil {
		return err
	}
	return syscall.Chown(dest, int(node.Uid), int(node.Gid))
}

func getMountInfo(mountinfo []*mount.Info, dir string) *mount.Info {
	for _, m := range mountinfo {
		if m.Mountpoint == dir {
			return m
		}
	}
	return nil
}

// Get the parent mount point of directory passed in as argument. Also return
// optional fields.
func getParentMount(rootfs string) (string, string, error) {
	var path string

	mountinfos, err := mount.GetMounts()
	if err != nil {
		return "", "", err
	}

	mountinfo := getMountInfo(mountinfos, rootfs)
	if mountinfo != nil {
		return rootfs, mountinfo.Optional, nil
	}

	path = rootfs
	for {
		path = filepath.Dir(path)

		mountinfo = getMountInfo(mountinfos, path)
		if mountinfo != nil {
			return path, mountinfo.Optional, nil
		}

		if path == "/" {
			break
		}
	}

	// If we are here, we did not find parent mount. Something is wrong.
	return "", "", fmt.Errorf("Could not find parent mount of %s", rootfs)
}

// Make parent mount private if it was shared
func rootfsParentMountPrivate(rootfs string) error {
	sharedMount := false

	parentMount, optionalOpts, err := getParentMount(rootfs)
	if err != nil {
		return err
	}

	optsSplit := strings.Split(optionalOpts, " ")
	for _, opt := range optsSplit {
		if strings.HasPrefix(opt, "shared:") {
			sharedMount = true
			break
		}
	}

	// Make parent mount PRIVATE if it was shared. It is needed for two
	// reasons. First of all pivot_root() will fail if parent mount is
	// shared. Secondly when we bind mount rootfs it will propagate to
	// parent namespace and we don't want that to happen.
	if sharedMount {
		return syscall.Mount("", parentMount, "", syscall.MS_PRIVATE, "")
	}

	return nil
}

func prepareRoot(config *configs.Config) error {
	flag := syscall.MS_SLAVE | syscall.MS_REC
	if config.RootPropagation != 0 {
		flag = config.RootPropagation
	}
	if err := syscall.Mount("", "/", "", uintptr(flag), ""); err != nil {
		return err
	}
	if config.NoPivotRoot {
		if err := rootfsParentMountPrivate(config.Rootfs); err != nil {
			return err
		}
	}

	return syscall.Mount(config.Rootfs, config.Rootfs, "bind", syscall.MS_BIND|syscall.MS_REC, "")
}

func setReadonly() error {
	return syscall.Mount("/", "/", "bind", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_REC, "")
}

func setupPtmx(config *configs.Config, console *linuxConsole) error {
	ptmx := filepath.Join(config.Rootfs, "dev/ptmx")
	if err := os.Remove(ptmx); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink("pts/ptmx", ptmx); err != nil {
		return fmt.Errorf("symlink dev ptmx %s", err)
	}
	if console != nil {
		return console.mount(config.Rootfs, config.MountLabel)
	}
	return nil
}

func pivotRoot(rootfs, pivotBaseDir string) (err error) {
	if pivotBaseDir == "" {
		pivotBaseDir = "/"
	}
	tmpDir := filepath.Join(rootfs, pivotBaseDir)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return fmt.Errorf("can't create tmp dir %s, error %v", tmpDir, err)
	}
	pivotDir, err := ioutil.TempDir(tmpDir, ".pivot_root")
	if err != nil {
		return fmt.Errorf("can't create pivot_root dir %s, error %v", pivotDir, err)
	}
	defer func() {
		errVal := os.Remove(pivotDir)
		if err == nil {
			err = errVal
		}
	}()
	if err := syscall.PivotRoot(rootfs, pivotDir); err != nil {
		// Make the parent mount private
		if err := rootfsParentMountPrivate(rootfs); err != nil {
			return err
		}
		// Try again
		if err := syscall.PivotRoot(rootfs, pivotDir); err != nil {
			return fmt.Errorf("pivot_root %s", err)
		}
	}
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir / %s", err)
	}
	// path to pivot dir now changed, update
	pivotDir = filepath.Join(pivotBaseDir, filepath.Base(pivotDir))

	// Make pivotDir rprivate to make sure any of the unmounts don't
	// propagate to parent.
	if err := syscall.Mount("", pivotDir, "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return err
	}

	if err := syscall.Unmount(pivotDir, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount pivot_root dir %s", err)
	}
	return nil
}

func msMoveRoot(rootfs string) error {
	if err := syscall.Mount(rootfs, "/", "", syscall.MS_MOVE, ""); err != nil {
		return err
	}
	if err := syscall.Chroot("."); err != nil {
		return err
	}
	return syscall.Chdir("/")
}

// createIfNotExists creates a file or a directory only if it does not already exist.
func createIfNotExists(path string, isDir bool) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			if isDir {
				return os.MkdirAll(path, 0755)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_CREATE, 0755)
			if err != nil {
				return err
			}
			f.Close()
		}
	}
	return nil
}

// remountReadonly will remount an existing mount point and ensure that it is read-only.
func remountReadonly(m *configs.Mount) error {
	var (
		dest  = m.Destination
		flags = m.Flags
	)
	for i := 0; i < 5; i++ {
		// There is a special case in the kernel for
		// MS_REMOUNT | MS_BIND, which allows us to change only the
		// flags even as an unprivileged user (i.e. user namespace)
		// assuming we don't drop any security related flags (nodev,
		// nosuid, etc.). So, let's use that case so that we can do
		// this re-mount without failing in a userns.
		flags |= unix.MS_REMOUNT | unix.MS_BIND | unix.MS_RDONLY
		if err := unix.Mount("", dest, "", uintptr(flags), ""); err != nil {
			switch err {
			case unix.EBUSY:
				time.Sleep(100 * time.Millisecond)
				continue
			default:
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unable to mount %s as readonly max retries reached", dest)
}

// maskPath masks the top of the specified path inside a container to avoid
// security issues from processes reading information from non-namespace aware
// mounts ( proc/kcore ).
// For files, maskPath bind mounts /dev/null over the top of the specified path.
// For directories, maskPath mounts read-only tmpfs over the top of the specified path.
func maskPath(path string) error {
	if err := syscall.Mount("/dev/null", path, "", syscall.MS_BIND, ""); err != nil && !os.IsNotExist(err) {
		if err == syscall.ENOTDIR {
			return syscall.Mount("tmpfs", path, "tmpfs", syscall.MS_RDONLY, "")
		}
		return err
	}
	return nil
}

// readonlyPath will make a path read only.
func readonlyPath(path string) error {
	if err := unix.Mount(path, path, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return unix.Mount(path, path, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_REC, "")
}

// writeSystemProperty writes the value to a path under /proc/sys as determined from the key.
// For e.g. net.ipv4.ip_forward translated to /proc/sys/net/ipv4/ip_forward.
func writeSystemProperty(key, value string) error {
	keyPath := strings.Replace(key, ".", "/", -1)
	return ioutil.WriteFile(path.Join("/proc/sys", keyPath), []byte(value), 0644)
}

func remount(m *configs.Mount, rootfs string) error {
	return utils.WithProcfd(rootfs, m.Destination, func(procfd string) error {
		return unix.Mount(m.Source, procfd, m.Device, uintptr(m.Flags|unix.MS_REMOUNT), "")
	})
}

// Do the mount operation followed by additional mounts required to take care
// of propagation flags. This will always be scoped inside the container rootfs.
func mountPropagate(m *configs.Mount, rootfs string, mountLabel string) error {
	var (
		data  = label.FormatMountLabel(m.Data, mountLabel)
		flags = m.Flags
	)

	// Delay mounting the filesystem read-only if we need to do further
	// operations on it. We need to set up files in "/dev" and tmpfs mounts may
	// need to be chmod-ed after mounting. The mount will be remounted ro later
	// in finalizeRootfs() if necessary.
	if utils.CleanPath(m.Destination) == "/dev" {
		flags &= ^unix.MS_RDONLY
	}

	// Because the destination is inside a container path which might be
	// mutating underneath us, we verify that we are actually going to mount
	// inside the container with WithProcfd() -- mounting through a procfd
	// mounts on the target.
	if err := utils.WithProcfd(rootfs, m.Destination, func(procfd string) error {
		return unix.Mount(m.Source, procfd, m.Device, uintptr(flags), data)
	}); err != nil {
		return fmt.Errorf("mount through procfd: %w", err)
	}
	// We have to apply mount propagation flags in a separate WithProcfd() call
	// because the previous call invalidates the passed procfd -- the mount
	// target needs to be re-opened.
	if err := utils.WithProcfd(rootfs, m.Destination, func(procfd string) error {
		for _, pflag := range m.PropagationFlags {
			if err := unix.Mount("", procfd, "", uintptr(pflag), ""); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("change mount propagation through procfd: %w", err)
	}
	return nil
}
