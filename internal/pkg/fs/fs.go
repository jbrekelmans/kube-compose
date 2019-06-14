package fs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// FileDescriptor is an abstraction of os.File to improve testability of code.
type FileDescriptor interface {
	io.ReadCloser
	Readdir(n int) ([]os.FileInfo, error)
}

// FileSystem is an abstraction of the file system to improve testability of code.
type FileSystem interface {
	EvalSymlinks(path string) (string, error)
	Mkdir(name string, perm os.FileMode) error
	MkdirAll(name string, perm os.FileMode) error
	Lstat(name string) (os.FileInfo, error)
	Open(name string) (FileDescriptor, error)
	Stat(name string) (os.FileInfo, error)
}

type osFileSystem struct {
}

func (fs *osFileSystem) EvalSymlinks(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

func (fs *osFileSystem) Mkdir(name string, perm os.FileMode) error {
	return os.Mkdir(name, perm)
}

func (fs *osFileSystem) MkdirAll(name string, perm os.FileMode) error {
	return os.MkdirAll(name, perm)
}

func (fs *osFileSystem) Lstat(name string) (os.FileInfo, error) {
	return os.Lstat(name)
}

func (fs *osFileSystem) Open(name string) (FileDescriptor, error) {
	return os.Open(name)
}

func (fs *osFileSystem) Stat(name string) (os.FileInfo, error) {
	return os.Stat(name)
}

var osfs FileSystem = &osFileSystem{}

// OSFileSystem returns a FileSystem instance that is backed by the os.
func OSFileSystem() FileSystem {
	return osfs
}

// VirtualFileSystem is a FileSystem with some helper methods useful for testing.
type VirtualFileSystem struct {
	cwd  string
	root *node
}

var (
	errBadMode           = fmt.Errorf("file has a bad mode (or operation is not supported on this file)")
	errIsDirDisagreement = fmt.Errorf("data contains a name X that is not a directory, but another name Y indicates " +
		"that X must be a directory")
	errTooManyLinks = fmt.Errorf("too many links")
)

func (fs *VirtualFileSystem) abs(name string) string {
	if name == "" || name[0] != '/' {
		return fs.cwd + name
	}
	return name
}

type findHelper struct {
	fs                   *VirtualFileSystem
	ignoreInjectedFaults bool
	links                int
	nameRem              string
	n                    *node
	resolveSymlinks      bool
}

func (f *findHelper) getChildN(nameComp string) (*node, error) {
	var childN *node
	if nameComp != "" {
		validateNameComp(nameComp)
		if (f.n.mode & os.ModeDir) == 0 {
			return nil, syscall.ENOTDIR
		}
		childN = f.n.dirLookup(nameComp)
		if childN == nil {
			return nil, os.ErrNotExist
		}
	}
	return childN, nil
}

func (f *findHelper) getNameComp(slashPos int) string {
	if slashPos < 0 {
		return f.nameRem
	}
	return f.nameRem[:slashPos]
}

func (f *findHelper) run() error {
	for f.nameRem != "" {
		if !f.ignoreInjectedFaults && f.n.err != nil {
			return f.n.err
		}
		slashPos := strings.IndexByte(f.nameRem, '/')
		nameComp := f.getNameComp(slashPos)
		childN, err := f.getChildN(nameComp)
		if err != nil {
			return err
		}
		f.updateNameRemFromSlashPos(slashPos)
		if nameComp != "" {
			err := f.updateFromChildN(childN)
			if err != nil {
				return err
			}
		}
	}
	if !f.ignoreInjectedFaults && f.n.err != nil {
		return f.n.err
	}
	return nil
}

func (f *findHelper) updateFromChildN(childN *node) error {
	if (childN.mode & os.ModeSymlink) != 0 {
		if f.resolveSymlinks {
			f.links++
			if f.links > 255 {
				return errTooManyLinks
			}
			target := childN.extra.([]byte)
			j := 0
			if len(target) > 0 && target[0] == '/' {
				// Absolute path
				j = 1
				f.n = f.fs.root
			}
			f.nameRem = string(target)[j:] + "/" + f.nameRem
		}
	} else {
		f.n = childN
	}
	return nil
}

func (f *findHelper) updateNameRemFromSlashPos(slashPos int) {
	if slashPos < 0 {
		f.nameRem = ""
	} else {
		f.nameRem = f.nameRem[slashPos+1:]
	}
}

func (fs *VirtualFileSystem) find(
	name string,
	ignoreInjectedFaults, resolveSymlinks bool) (n *node, nameRem string, err error) {
	f := findHelper{
		fs:                   fs,
		ignoreInjectedFaults: ignoreInjectedFaults,
		nameRem:              fs.abs(name)[1:],
		n:                    fs.root,
		resolveSymlinks:      resolveSymlinks,
	}
	err = f.run()
	n = f.n
	nameRem = f.nameRem
	return
}

func validateNameComp(nameComp string) {
	if nameComp == "." || nameComp == ".." {
		panic(fmt.Errorf("name must not contain '//' and must not have a path component that is one of  '..' and '.'"))
	}
}

func (fs *VirtualFileSystem) createChildren(n *node, nameRem string, vfile *VirtualFile) {
	for {
		var nameComp string
		slashPos := strings.IndexByte(nameRem, '/')
		if slashPos < 0 {
			nameComp = nameRem
		} else {
			nameComp = nameRem[:slashPos]
		}
		if nameComp != "" {
			validateNameComp(nameComp)
			var childN *node
			if slashPos < 0 {
				// initialize file or directory as per VirtualFile
				childN = &node{
					err:  vfile.Error,
					mode: vfile.Mode,
					name: nameComp,
				}
				if (vfile.Mode & os.ModeDir) == 0 {
					childN.extra = vfile.Content
				} else {
					childN.extra = []*node{}
				}
				n.dirAppend(childN)
				return
			}
			// initialize directory with defaults
			childN = newDirNode(
				os.ModeDir,
				nameComp,
			)
			n.dirAppend(childN)
			n = childN
		}
		if slashPos < 0 {
			break
		}
		nameRem = nameRem[slashPos+1:]
	}
}

// VirtualFile is a helper struct used to initialize a file, directory or other type of file in a virtual file system.
// If Error is set then all file system operations will produce an error when the file is accessed. If Mode is a regular
// file then Content is the content of that file. If Mode is Symlink then Content is the location of the Symlink.
type VirtualFile struct {
	Content []byte
	Mode    os.FileMode
	Error   error
}

// NewVirtualFileSystem creates a mock file system based on the provided data.
func NewVirtualFileSystem(data map[string]VirtualFile) *VirtualFileSystem {
	fs := &VirtualFileSystem{
		cwd: "/",
		root: newDirNode(
			0,
			"/",
		),
	}
	for name, vfile := range data {
		fs.Set(name, vfile)
	}
	return fs
}

// Set sets or updates the file at name. If one of the parents of name exists and is not a directory then the error ENOTDIR is returned. If
// a file already exists at name and it is a directory and vfile is not a directory (or vice versa) then an error is thrown. Otherwise, if a
// file already exists at name its attributes, injected fault, symlink target or regular file contents are updated with the values from
// vfile.
func (fs *VirtualFileSystem) Set(name string, vfile VirtualFile) {
	var flag os.FileMode
	switch {
	case vfile.Mode.IsDir():
		flag = os.ModeDir
	case (vfile.Mode & os.ModeSymlink) != 0:
		flag = os.ModeSymlink
	case vfile.Mode.IsRegular():
		flag = 0
	}
	if (vfile.Mode & (os.ModeType &^ flag)) != 0 {
		panic(errBadMode)
	}
	n, nameRem, err := fs.find(name, true, false)
	if err == syscall.ENOTDIR {
		panic(errIsDirDisagreement)
	}
	if nameRem != "" {
		fs.createChildren(n, nameRem, &vfile)
	} else {
		nodeIsDir := (n.mode & os.ModeDir) != 0
		vfileIsDir := (vfile.Mode & os.ModeDir) != 0
		if nodeIsDir != vfileIsDir {
			panic(errIsDirDisagreement)
		}
		n.mode = vfile.Mode
		n.err = vfile.Error
		if !vfileIsDir {
			n.extra = vfile.Content
		}
	}
}

type virtualFileDescriptor struct {
	node    *node
	readPos int
}

func (r *virtualFileDescriptor) Close() error {
	return nil
}

func (r *virtualFileDescriptor) Read(p []byte) (n int, err error) {
	if !r.node.mode.IsRegular() {
		err = errBadMode
		return
	}
	if len(p) > 0 {
		fileContents := r.node.extra.([]byte)
		n = copy(p, fileContents[r.readPos:])
		r.readPos += n
		if n == 0 {
			err = io.EOF
		}
	}
	return
}

func (r *virtualFileDescriptor) Readdir(n int) ([]os.FileInfo, error) {
	if !r.node.mode.IsDir() {
		return nil, syscall.ENOTDIR
	}
	if n > 0 {
		panic(fmt.Errorf("not supported"))
	}
	dir := r.node.extra.([]*node)
	if len(dir) == 0 {
		return nil, nil
	}
	fileInfoSlice := make([]os.FileInfo, len(dir))
	for i := 0; i < len(dir); i++ {
		fileInfoSlice[i] = dir[i]
	}
	return fileInfoSlice, nil
}

func trimTrailingSlashes(name string) string {
	n := len(name)
	for n > 0 && name[n-1] == '/' {
		n--
	}
	return name[:n]
}

func (fs *VirtualFileSystem) Open(name string) (FileDescriptor, error) {
	node, _, err := fs.find(name, false, true)
	if err != nil {
		return nil, err
	}
	return &virtualFileDescriptor{
		node: node,
	}, nil
}

func (fs *VirtualFileSystem) Stat(name string) (os.FileInfo, error) {
	n, _, err := fs.find(name, false, true)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// IsPathSeparatorWindows returns true if and only if b is the ASCII code of a forward or backward slash.
func IsPathSeparatorWindows(b byte) bool {
	return b == '/' || b == '\\'
}
