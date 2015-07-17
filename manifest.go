package continuity

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/docker/distribution/digest"
	"github.com/stevvooe/continuity/protos"
)

// BuildManifest creates the manifest for the root directory. includeFn should
// return nil for files that should be included in the manifest. The function
// is called with the unmodified arguments of filepath.Walk.
func BuildManifest(root string, includeFn filepath.WalkFunc) (*protos.Manifest, error) {
	entriesByPath := map[string]*protos.Entry{}
	hardlinks := map[hardlinkKey][]*protos.Entry{}

	gi, err := getGroupIndex()
	if err != nil {
		return nil, err
	}

	// normalize to absolute path
	root, err = filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, err
	}

	if err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if p == root {
			// skip the root
			return nil
		}

		sanitized, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		sanitized = filepath.Clean(sanitized)

		entry := protos.Entry{
			Path: sanitized,
			Mode: fi.Mode(),
		}

		sysStat := fi.Sys().(*syscall.Stat_t)

		uid, gid := sysStat.Uid, sysStat.Gid

		u, err := user.LookupId(fmt.Sprint(uid))
		if err != nil {
			return err
		}
		entry.User = u.Username
		entry.Uid = fmt.Sprint(uid)
		entry.Group = gi.byGID[int(gid)].name
		entry.Gid = fmt.Sprint(gid)

		// TODO(stevvooe): Handle xattrs.
		// TODO(stevvooe): Handle ads.

		if fi.Mode().IsRegular() && sysStat.Nlink < 2 {
			dgst, err := hashPath(p)
			if err != nil {
				return err
			}

			entry.Digest = append(entry.Digest, dgst.String())
		}

		if fi.Mode().IsRegular() && sysStat.Nlink > 1 { // hard links
			// Properties of hard links:
			//	- nlinks > 1 (not all filesystems)
			//	- identical dev and inode number for two files
			//	- consider the file with the earlier ctime the "canonical" path
			//
			// How will this be done?
			//	- check nlinks to detect hard links
			//		-> add them to map by dev, inode
			//	- hard links are still set as regular files with target set
			//	- rather than recalculate digest, use other entry
			//	- defer addition to entries until after all entries are seen
			key := hardlinkKey{dev: sysStat.Dev, inode: sysStat.Ino}

			// add the hardlink
			hardlinks[key] = append(hardlinks[key], &entry)

			// TODO(stevvooe): Possibly use os.SameFile here?

			return nil // hardlinks are postprocessed, so we exit
		}

		if fi.Mode()&os.ModeSymlink != 0 {
			// Walk does not follow symbolic links, but os.Stat does. Simply
			// stat the path to get the link target. The target must be in the
			// contianer bundle. If not, the target will be unspecified and
			// the link will be considered "broken".

			// TODO(stevvooe): Include a leading slash on the symlink to
			// indicate whether it is absolute. Even though the bundle may be
			// unpacked at some other root, we treat the bundle root as the
			// absolute link anchor.

			target, err := os.Readlink(p)
			if err != nil {
				return err
			}

			fmt.Println(p, target, filepath.Join(p, target))
			if filepath.IsAbs(target) {
				// When path is absolute, we make it relative to the bundle root.
				target, err = filepath.Rel(root, target)
				if err != nil {
					return err
				}
			} else {
				// make sure the target is contained in the root.
			}

			entry.Target = target
		}

		if fi.Mode()&os.ModeNamedPipe != 0 {
			// Everything needed to rebuild a pipe is included in the mode.
		}

		if fi.Mode()&os.ModeDevice != 0 {
			// character and block devices merely need to recover the
			// major/minor device number.
			entry.Major = uint32(major(uint(sysStat.Rdev)))
			entry.Minor = uint32(minor(uint(sysStat.Rdev)))
		}

		if fi.Mode()&os.ModeSocket != 0 {
			return nil // sockets are skipped, no point
		}

		entriesByPath[p] = &entry

		return nil
	}); err != nil {
		return nil, err
	}

	// process the groups of hardlinks
	for pair, linked := range hardlinks {
		if len(linked) < 1 {
			return nil, fmt.Errorf("no hardlink entrys for dev, inode pair: %#v", pair)
		}

		// a canonical hardlink target is selected by sort position to ensure
		// the same file will always be used as the link target.
		sort.Sort(byPath(linked))

		canonical, rest := linked[0], linked[1:]

		dgst, err := hashPath(filepath.Join(root, canonical.Path))
		if err != nil {
			return nil, err
		}

		// canonical gets appended like a regular file.
		canonical.Digest = append(canonical.Digest, dgst.String())
		entriesByPath[canonical.Path] = canonical

		// process the links.
		for _, link := range rest {
			// a hardlink is a regular file with a target instead of a digest.
			// We can just set the target from the canonical path since
			// hardlinks are alwas
			link.Target = canonical.Path
			entriesByPath[link.Path] = link
		}
	}

	var entries []*protos.Entry
	for _, entry := range entriesByPath {
		entries = append(entries, entry)
	}

	sort.Sort(byPath(entries))

	return &protos.Manifest{
		Entry: entries,
	}, nil
}

// hardlinkKey provides a tuple-key for managing hardlinks.
type hardlinkKey struct {
	dev   int32
	inode uint64
}

func hashPath(p string) (digest.Digest, error) {
	digester := digest.Canonical.New() // TODO(stevvooe): Make this configurable.

	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(digester.Hash(), f); err != nil {
		return "", err
	}

	return digester.Digest(), nil

}

type byPath []*protos.Entry

func (bp byPath) Len() int           { return len(bp) }
func (bp byPath) Swap(i, j int)      { bp[i], bp[j] = bp[j], bp[i] }
func (bp byPath) Less(i, j int) bool { return bp[i].Path < bp[j].Path }
