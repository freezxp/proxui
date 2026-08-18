package sshclient

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/freezxp/proxui/internal/domain/shell"
)

// remoteFS is the SFTP subsystem of a connection (SSH-09).
type remoteFS struct{ client *sftp.Client }

func newRemoteFS(client *ssh.Client) (*remoteFS, error) {
	// MaxPacket at 32 KiB is what most servers negotiate happily; the default
	// is smaller and makes a large upload noticeably slower.
	c, err := sftp.NewClient(client, sftp.MaxPacket(32*1024))
	if err != nil {
		// A guest with `Subsystem sftp` commented out fails here, and the
		// terminal beside it still works perfectly - so this is reported as a
		// missing feature rather than as a broken session.
		return nil, fmt.Errorf("%w: %v", shell.ErrFilesUnavailable, err)
	}
	return &remoteFS{client: c}, nil
}

// Home is where the browser opens: the SFTP server starts in the account's own
// home directory, so asking it beats guessing /home/<user> - which is wrong for
// root and for every account with a relocated home.
func (r *remoteFS) Home(context.Context) (string, error) {
	wd, err := r.client.Getwd()
	if err != nil || wd == "" {
		return "/", err
	}
	return wd, nil
}

// List reads a directory, directories first and then by name - the order a
// file browser is expected to show rather than whatever order the server
// happened to walk the inodes in.
func (r *remoteFS) List(_ context.Context, dir string) ([]shell.FileEntry, error) {
	infos, err := r.client.ReadDir(dir)
	if err != nil {
		return nil, translate(err)
	}

	entries := make([]shell.FileEntry, 0, len(infos))
	for _, info := range infos {
		entries = append(entries, r.entry(dir, info))
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, nil
}

func (r *remoteFS) Stat(_ context.Context, p string) (shell.FileEntry, error) {
	info, err := r.client.Stat(p)
	if err != nil {
		return shell.FileEntry{}, translate(err)
	}
	return r.entry(path.Dir(p), info), nil
}

func (r *remoteFS) entry(dir string, info fs.FileInfo) shell.FileEntry {
	full := path.Join(dir, info.Name())
	e := shell.FileEntry{
		Name:     info.Name(),
		Path:     full,
		Size:     info.Size(),
		Mode:     info.Mode().String(),
		ModeBits: uint32(info.Mode().Perm()),
		IsDir:    info.IsDir(),
		IsLink:   info.Mode()&os.ModeSymlink != 0,
		ModTime:  info.ModTime().UTC(),
	}
	// Ownership comes back as numbers. Resolving them to names would mean
	// reading the guest's passwd database over SFTP for every listing, which
	// is a lot of round trips for a column; the numbers are what `ls -n`
	// shows and are unambiguous.
	if stat, ok := info.Sys().(*sftp.FileStat); ok && stat != nil {
		e.Owner = strconv.FormatUint(uint64(stat.UID), 10)
		e.Group = strconv.FormatUint(uint64(stat.GID), 10)
	}
	if e.IsLink {
		if target, err := r.client.ReadLink(full); err == nil {
			e.Target = target
			// A link to a directory should behave like a directory in the
			// browser; otherwise /var/log -> /mnt/log is a dead end.
			if info, err := r.client.Stat(full); err == nil && info.IsDir() {
				e.IsDir = true
			}
		}
	}
	return e
}

func (r *remoteFS) Open(_ context.Context, p string) (io.ReadCloser, int64, error) {
	info, err := r.client.Stat(p)
	if err != nil {
		return nil, 0, translate(err)
	}
	if info.IsDir() {
		return nil, 0, fmt.Errorf("%w: that is a directory", shell.ErrBadPath)
	}
	f, err := r.client.Open(p)
	if err != nil {
		return nil, 0, translate(err)
	}
	return f, info.Size(), nil
}

func (r *remoteFS) Create(_ context.Context, p string) (io.WriteCloser, error) {
	f, err := r.client.Create(p)
	if err != nil {
		return nil, translate(err)
	}
	return f, nil
}

// Append opens a file at its end, creating it if absent (SSH-12). Adding a
// key to authorized_keys must not be able to lose the keys already in it,
// which a truncating open can do.
func (r *remoteFS) Append(_ context.Context, p string) (io.WriteCloser, error) {
	f, err := r.client.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND)
	if err != nil {
		return nil, translate(err)
	}
	// The append flag alone is not enough. SFTP writes carry an explicit
	// offset, and a server that ignores the flag takes the client's offset -
	// zero on a freshly opened handle - and overwrites from the start of the
	// file. Seeking to the end makes both kinds of server do the same thing.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		_ = f.Close()
		return nil, translate(err)
	}
	return f, nil
}

func (r *remoteFS) Mkdir(_ context.Context, p string) error {
	return translate(r.client.Mkdir(p))
}

// Remove deletes a file, or an empty directory. Recursive deletion is
// deliberately absent: a file browser that can erase a subtree by mis-click is
// a different tool, and `rm -rf` in the terminal beside it is explicit about
// what it is doing.
func (r *remoteFS) Remove(_ context.Context, p string) error {
	info, err := r.client.Lstat(p)
	if err != nil {
		return translate(err)
	}
	if info.IsDir() {
		return translate(r.client.RemoveDirectory(p))
	}
	return translate(r.client.Remove(p))
}

func (r *remoteFS) Rename(_ context.Context, from, to string) error {
	return translate(r.client.Rename(from, to))
}

func (r *remoteFS) Chmod(_ context.Context, p string, mode uint32) error {
	return translate(r.client.Chmod(p, fs.FileMode(mode).Perm()))
}

// Close releases the subsystem. The connection under it stays open: the
// terminal is still using it.
func (r *remoteFS) Close() error { return r.client.Close() }

// translate turns SFTP status codes into errors the API layer can map onto
// status codes, so a permission denial reads as 403 rather than as a 500.
func translate(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case os.IsNotExist(err):
		return fmt.Errorf("%w: %v", fs.ErrNotExist, firstLine(err.Error()))
	case os.IsPermission(err):
		return fmt.Errorf("%w: %v", fs.ErrPermission, firstLine(err.Error()))
	case os.IsExist(err):
		return fmt.Errorf("%w: %v", fs.ErrExist, firstLine(err.Error()))
	default:
		return err
	}
}
