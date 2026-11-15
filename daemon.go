package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	daemonSocketPath         = "/run/agentbox/agentbox.sock"
	firstExtraFileDescriptor = 3
	maxRequestSize           = 64 * 1024
	maxDaemonClients         = 8
)

type btrfsRequest struct {
	Op     string `json:"op"`
	Source string `json:"source,omitempty"`
	Path   string `json:"path"`
}

type daemonResponse struct {
	Error string `json:"error,omitempty"`
}

type authorizedPaths struct {
	Source            *os.File
	DestinationParent *os.File
	Subvolume         *os.File
}

func (p *authorizedPaths) files() []*os.File {
	var files []*os.File
	for _, file := range []*os.File{p.Source, p.DestinationParent, p.Subvolume} {
		if file != nil {
			files = append(files, file)
		}
	}
	return files
}

type childFiles []*os.File

func (f childFiles) path(file *os.File) (string, error) {
	for index, inherited := range f {
		if inherited == file {
			return fmt.Sprintf("/proc/self/fd/%d", firstExtraFileDescriptor+index), nil
		}
	}
	return "", errors.New("file is not inherited by the child process")
}

func requestPrivileged(req btrfsRequest) error {
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: daemonSocketPath, Net: "unix"})
	if err != nil {
		return fmt.Errorf("connect to btrfs daemon at %s: %w (start it with `sudo agentbox daemon`)", daemonSocketPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Minute))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("send request to btrfs daemon: %w", err)
	}
	var response daemonResponse
	if err := json.NewDecoder(io.LimitReader(conn, maxRequestSize)).Decode(&response); err != nil {
		return fmt.Errorf("read response from btrfs daemon: %w", err)
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	return nil
}

func cmdDaemon(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: agentbox daemon")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("the btrfs daemon must run as root; start it with `sudo agentbox daemon`")
	}
	return serveDaemon()
}

func serveDaemon() error {
	if err := prepareDaemonSocketDir(); err != nil {
		return err
	}
	btrfsPath, err := trustedBtrfsPath()
	if err != nil {
		return err
	}
	addr := &net.UnixAddr{Name: daemonSocketPath, Net: "unix"}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w (remove a stale socket only after verifying no daemon is running)", daemonSocketPath, err)
	}
	defer listener.Close()
	if err := os.Chmod(daemonSocketPath, 0o666); err != nil {
		return err
	}
	defer os.Remove(daemonSocketPath)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	shutdown := make(chan struct{})
	go func() {
		<-signals
		close(shutdown)
		_ = listener.Close()
	}()

	var handlers sync.WaitGroup
	defer handlers.Wait()
	slots := make(chan struct{}, maxDaemonClients)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		case <-shutdown:
			_ = conn.Close()
			return nil
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() { <-slots }()
			defer conn.Close()
			if err := handleDaemonConnection(conn, btrfsPath); err != nil {
				fmt.Fprintln(os.Stderr, "agentbox daemon:", err)
			}
		}()
	}
}

func prepareDaemonSocketDir() error {
	const dir = "/run/agentbox"
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o001 == 0 {
		return fmt.Errorf("%s must be root-owned, searchable by clients, and not writable by other users", dir)
	}
	return nil
}

func trustedBtrfsPath() (string, error) {
	const rootPath = "/usr/sbin:/usr/bin:/sbin:/bin"
	found := ""
	for _, dir := range strings.Split(rootPath, ":") {
		path := filepath.Join(dir, "btrfs")
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			return "", fmt.Errorf("refusing untrusted btrfs executable at %s", path)
		}
		found = path
		break
	}
	if found == "" {
		return "", errors.New("btrfs executable not found in standard system directories")
	}
	return found, nil
}

func handleDaemonConnection(conn *net.UnixConn, btrfsPath string) error {
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	request, err := readBtrfsRequest(conn)
	if err != nil {
		return writeDaemonResponse(conn, err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Minute))
	cred, err := peerCredentials(conn)
	if err == nil {
		err = executeBtrfsRequest(request, cred, btrfsPath)
	}
	return writeDaemonResponse(conn, err)
}

func readBtrfsRequest(conn *net.UnixConn) (btrfsRequest, error) {
	var req btrfsRequest
	dec := json.NewDecoder(io.LimitReader(conn, maxRequestSize))
	if err := dec.Decode(&req); err != nil {
		return req, fmt.Errorf("invalid request: %w", err)
	}
	switch req.Op {
	case "snapshot":
		if req.Source == "" || req.Path == "" {
			return req, errors.New("snapshot requires source and path")
		}
	case "create":
		if req.Path == "" || req.Source != "" {
			return req, errors.New("create requires path only")
		}
	case "delete":
		if req.Path == "" || req.Source != "" {
			return req, errors.New("delete requires path only")
		}
	default:
		return req, fmt.Errorf("unsupported operation %q", req.Op)
	}
	return req, nil
}

func writeDaemonResponse(conn *net.UnixConn, err error) error {
	response := daemonResponse{}
	if err != nil {
		response.Error = err.Error()
	}
	return json.NewEncoder(conn).Encode(response)
}

func peerCredentials(conn *net.UnixConn) (*syscall.Ucred, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *syscall.Ucred
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		cred, controlErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	return cred, controlErr
}

func executeBtrfsRequest(req btrfsRequest, cred *syscall.Ucred, btrfsPath string) error {
	groups, err := peerGroups(cred)
	if err != nil {
		return err
	}
	paths, err := authorizePaths(req, cred, groups)
	if err != nil {
		return err
	}
	defer closeFiles(paths.files())

	if req.Op == "snapshot" {
		return executeSnapshotRequest(req, btrfsPath, paths)
	}
	if req.Op == "create" {
		files := childFiles(paths.files())
		parentPath, err := files.path(paths.DestinationParent)
		if err != nil {
			return err
		}
		subvolumePath := filepath.Join(parentPath, filepath.Base(filepath.Clean(req.Path)))
		if err := runBtrfsCommand(btrfsPath, files, "subvolume", "create", subvolumePath); err != nil {
			return err
		}
		if err := unix.Fchownat(int(paths.DestinationParent.Fd()), filepath.Base(filepath.Clean(req.Path)), int(cred.Uid), int(cred.Gid), 0); err != nil {
			deleteErr := runBtrfsCommand(btrfsPath, files, "subvolume", "delete", subvolumePath)
			return errors.Join(fmt.Errorf("set subvolume ownership: %w", err), deleteErr)
		}
		return nil
	}
	files := childFiles(paths.files())
	subvolumePath, err := files.path(paths.Subvolume)
	if err != nil {
		return err
	}
	return runBtrfsCommand(btrfsPath, files, "subvolume", "delete", subvolumePath)
}

func executeSnapshotRequest(req btrfsRequest, btrfsPath string, paths *authorizedPaths) error {
	files := childFiles(paths.files())
	sourcePath, err := files.path(paths.Source)
	if err != nil {
		return err
	}
	parentFD := int(paths.DestinationParent.Fd())
	tempName, tempDir, err := createStagingDirectory(parentFD)
	if err != nil {
		return err
	}
	defer tempDir.Close()

	files = append(files, tempDir)
	stagingPath, err := files.path(tempDir)
	if err != nil {
		cleanupErr := removeStagingDirectory(parentFD, tempName, int(tempDir.Fd()))
		return errors.Join(err, cleanupErr)
	}
	stagingPath = filepath.Join(stagingPath, "snapshot")
	operationErr := runBtrfsCommand(btrfsPath, files,
		"subvolume", "snapshot", sourcePath, stagingPath)
	if operationErr == nil {
		destName := filepath.Base(filepath.Clean(req.Path))
		if err := unix.Renameat2(int(tempDir.Fd()), "snapshot", parentFD, destName, unix.RENAME_NOREPLACE); err != nil {
			operationErr = fmt.Errorf("publish snapshot: %w", err)
		}
	}
	if operationErr != nil {
		if err := deleteStagedSnapshot(btrfsPath, files, stagingPath, int(tempDir.Fd())); err != nil {
			operationErr = errors.Join(operationErr, fmt.Errorf("remove staged snapshot: %w", err))
		}
	}
	if err := removeStagingDirectory(parentFD, tempName, int(tempDir.Fd())); err != nil {
		operationErr = errors.Join(operationErr, err)
	}
	return operationErr
}

func createStagingDirectory(parentFD int) (string, *os.File, error) {
	for range 5 {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		name := ".agentbox-" + hex.EncodeToString(random[:])
		if err := unix.Mkdirat(parentFD, name, 0o700); err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return "", nil, fmt.Errorf("create snapshot staging directory: %w", err)
		}
		fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			_ = unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
			return "", nil, fmt.Errorf("open snapshot staging directory: %w", err)
		}
		if err := unix.Fchmod(fd, 0o700); err != nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR)
			return "", nil, fmt.Errorf("secure snapshot staging directory: %w", err)
		}
		return name, os.NewFile(uintptr(fd), name), nil
	}
	return "", nil, errors.New("could not allocate a unique snapshot staging directory")
}

func deleteStagedSnapshot(btrfsPath string, files childFiles, stagingPath string, dirFD int) error {
	var info unix.Stat_t
	if err := unix.Fstatat(dirFD, "snapshot", &info, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("staged snapshot path is not a directory")
	}
	return runBtrfsCommand(btrfsPath, files, "subvolume", "delete", stagingPath)
}

func removeStagingDirectory(parentFD int, name string, dirFD int) error {
	var opened, entry unix.Stat_t
	if err := unix.Fstat(dirFD, &opened); err != nil {
		return fmt.Errorf("inspect snapshot staging directory: %w", err)
	}
	if err := unix.Fstatat(parentFD, name, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("inspect snapshot staging path: %w", err)
	}
	if opened.Dev != entry.Dev || opened.Ino != entry.Ino {
		return errors.New("snapshot staging directory was moved or replaced")
	}
	if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("remove snapshot staging directory: %w", err)
	}
	return nil
}

func runBtrfsCommand(btrfsPath string, files childFiles, args ...string) error {
	cmd := exec.Command(btrfsPath, args...)
	cmd.ExtraFiles = []*os.File(files)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stdout.String() + stderr.String())
		if message == "" {
			return fmt.Errorf("btrfs %s failed: %w", strings.Join(args[:2], " "), err)
		}
		return fmt.Errorf("btrfs %s failed: %s", strings.Join(args[:2], " "), message)
	}
	return nil
}

func peerGroups(cred *syscall.Ucred) ([]uint32, error) {
	status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(int(cred.Pid)), "status"))
	if err != nil {
		return nil, fmt.Errorf("read client credentials: %w", err)
	}
	var uid, gid uint32
	var groups []uint32
	var gotUID, gotGID, gotGroups bool
	for _, line := range strings.Split(string(status), "\n") {
		switch {
		case strings.HasPrefix(line, "Uid:"):
			fields := strings.Fields(strings.TrimPrefix(line, "Uid:"))
			if len(fields) >= 2 {
				value, err := strconv.ParseUint(fields[1], 10, 32)
				if err != nil {
					return nil, err
				}
				uid, gotUID = uint32(value), true
			}
		case strings.HasPrefix(line, "Gid:"):
			fields := strings.Fields(strings.TrimPrefix(line, "Gid:"))
			if len(fields) >= 2 {
				value, err := strconv.ParseUint(fields[1], 10, 32)
				if err != nil {
					return nil, err
				}
				gid, gotGID = uint32(value), true
			}
		case strings.HasPrefix(line, "Groups:"):
			for _, field := range strings.Fields(strings.TrimPrefix(line, "Groups:")) {
				value, err := strconv.ParseUint(field, 10, 32)
				if err != nil {
					return nil, err
				}
				groups = append(groups, uint32(value))
			}
			gotGroups = true
		}
	}
	if !gotUID || !gotGID || !gotGroups || uid != cred.Uid || gid != cred.Gid {
		return nil, errors.New("client process credentials changed or could not be verified")
	}
	return groups, nil
}

func authorizePaths(req btrfsRequest, cred *syscall.Ucred, groups []uint32) (*authorizedPaths, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parent := os.NewFile(uintptr(pair[0]), "authorization-parent")
	child := os.NewFile(uintptr(pair[1]), "authorization-child")
	defer parent.Close()
	defer child.Close()

	childArgs := []string{"__authorize-fds", req.Op, req.Path}
	if req.Op == "snapshot" {
		childArgs = append(childArgs, req.Source)
	}
	cmd := exec.Command(executable, childArgs...)
	cmd.ExtraFiles = []*os.File{child}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: cred.Uid, Gid: cred.Gid, Groups: groups},
	}
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stdout.String() + stderr.String())
		if message == "" {
			return nil, fmt.Errorf("client access check failed: %w", err)
		}
		return nil, fmt.Errorf("client access check failed: %s", message)
	}

	oob := make([]byte, syscall.CmsgSpace(2*4))
	data := make([]byte, 1)
	_, oobn, _, _, err := syscall.Recvmsg(int(parent.Fd()), data, oob, 0)
	if err != nil {
		return nil, fmt.Errorf("receive authorized directory handles: %w", err)
	}
	messages, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(messages) != 1 {
		return nil, errors.New("invalid authorized directory handles")
	}
	rights, err := syscall.ParseUnixRights(&messages[0])
	if err != nil {
		return nil, err
	}
	expected := 1
	if req.Op == "snapshot" {
		expected = 2
	}
	if len(rights) != expected {
		for _, fd := range rights {
			_ = syscall.Close(fd)
		}
		return nil, errors.New("unexpected number of authorized directory handles")
	}
	files := make([]*os.File, len(rights))
	for i, fd := range rights {
		files[i] = os.NewFile(uintptr(fd), "authorized-directory")
	}
	paths := &authorizedPaths{}
	switch req.Op {
	case "snapshot":
		paths.Source, paths.DestinationParent = files[0], files[1]
	case "create":
		paths.DestinationParent = files[0]
	default:
		paths.Subvolume = files[0]
	}
	return paths, nil
}

func runAuthorizeChild(args []string) error {
	if len(args) < 2 || len(args) > 3 {
		return errors.New("invalid authorization helper arguments")
	}
	op, path := args[0], args[1]
	socket := os.NewFile(firstExtraFileDescriptor, "authorization-socket")
	if socket == nil {
		return errors.New("authorization socket is missing")
	}
	defer socket.Close()

	var files []*os.File
	switch op {
	case "snapshot":
		if len(args) != 3 {
			return errors.New("snapshot authorization requires source and destination")
		}
		source, err := openAuthorizedDirectory(args[2])
		if err != nil {
			return err
		}
		files = append(files, source)
		destParent, err := openAuthorizedParent(path)
		if err != nil {
			closeFiles(files)
			return err
		}
		files = append(files, destParent)
	case "create":
		if len(args) != 2 {
			return errors.New("create authorization requires a path")
		}
		parent, err := openAuthorizedParent(path)
		if err != nil {
			return err
		}
		files = append(files, parent)
	case "delete":
		if len(args) != 2 {
			return errors.New("delete authorization requires a path")
		}
		subvolume, err := openAuthorizedSubvolume(path)
		if err != nil {
			return err
		}
		files = append(files, subvolume)
	default:
		return errors.New("unsupported authorization operation")
	}
	defer closeFiles(files)
	rights := make([]int, len(files))
	for i, file := range files {
		rights[i] = int(file.Fd())
	}
	return syscall.Sendmsg(int(socket.Fd()), []byte{1}, syscall.UnixRights(rights...), nil, 0)
}

func openAuthorizedDirectory(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("path must be absolute: %s", path)
	}
	fd, err := syscall.Open(filepath.Clean(path), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("client cannot access directory %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), filepath.Clean(path))
	info, err := file.Stat()
	if err != nil || !info.IsDir() {
		file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s is not a directory", path)
	}
	return file, nil
}

func openAuthorizedParent(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("path must be absolute: %s", path)
	}
	path = filepath.Clean(path)
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) || base == ".." {
		return nil, fmt.Errorf("invalid subvolume path %s", path)
	}
	parentPath := filepath.Dir(path)
	fd, err := syscall.Open(parentPath, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("client cannot access parent directory %s: %w", parentPath, err)
	}
	parent := os.NewFile(uintptr(fd), parentPath)
	if err := probeCreatePermission(parent); err != nil {
		parent.Close()
		return nil, fmt.Errorf("client cannot create entries in %s: %w", parentPath, err)
	}
	childPath := fmt.Sprintf("/proc/self/fd/%d/%s", parent.Fd(), base)
	_, err = os.Lstat(childPath)
	if err == nil {
		parent.Close()
		return nil, fmt.Errorf("destination already exists: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		parent.Close()
		return nil, err
	}
	return parent, nil
}

func openAuthorizedSubvolume(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("path must be absolute: %s", path)
	}
	path = filepath.Clean(path)
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) || base == ".." {
		return nil, fmt.Errorf("invalid subvolume path %s", path)
	}
	parentPath := filepath.Dir(path)
	parentFD, err := syscall.Open(parentPath, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("client cannot access parent directory %s: %w", parentPath, err)
	}
	parent := os.NewFile(uintptr(parentFD), parentPath)
	defer parent.Close()
	if err := probeCreatePermission(parent); err != nil {
		return nil, fmt.Errorf("client cannot delete entries in %s: %w", parentPath, err)
	}
	childPath := fmt.Sprintf("/proc/self/fd/%d/%s", parent.Fd(), base)
	info, err := os.Lstat(childPath)
	if err != nil {
		return nil, fmt.Errorf("client cannot access subvolume %s: %w", path, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("subvolume path is not a directory: %s", path)
	}
	parentInfo, err := parent.Stat()
	if err != nil {
		return nil, err
	}
	if parentInfo.Mode()&os.ModeSticky != 0 && os.Geteuid() != 0 {
		parentStat, parentOK := parentInfo.Sys().(*syscall.Stat_t)
		targetStat, targetOK := info.Sys().(*syscall.Stat_t)
		if !parentOK || !targetOK {
			return nil, errors.New("could not verify sticky-directory ownership")
		}
		uid := uint32(os.Geteuid())
		if uid != parentStat.Uid && uid != targetStat.Uid {
			return nil, fmt.Errorf("client may not delete %s from sticky directory %s", path, parentPath)
		}
	}
	fd, err := syscall.Open(childPath, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("client cannot access subvolume %s: %w", path, err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

func probeCreatePermission(parent *os.File) error {
	probe, err := os.CreateTemp(fmt.Sprintf("/proc/self/fd/%d", parent.Fd()), ".agentbox-access-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func closeFiles(files []*os.File) {
	for _, file := range files {
		if file != nil {
			_ = file.Close()
		}
	}
}
