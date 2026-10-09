package prismcontainer

// The build context copy.
//
// Podman reads the build context while the build runs, and buildah opens
// the context path again at each COPY. If podman reads the worktree, the
// agent can replace a context directory with a symlink to a host path
// during a RUN step. A later COPY then reads that host path. Thus prism
// copies the context and the Containerfile into a directory that no sandbox
// can write, and podman builds from the copy.
//
// The copy reads the worktree through os.Root, so no path that it opens
// can resolve outside the worktree, whatever the agent changes during the
// copy.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/prismatic-koi/prism/internal/container"
)

// MaxContextBytes is the largest build context, in file bytes, that prism
// copies. A sparse file in the worktree can claim any size, and the copy
// writes every byte.
const MaxContextBytes int64 = 4 << 30

// maxSmallFileBytes limits the Containerfile and the ignore files.
const maxSmallFileBytes int64 = 1 << 20

// errContextChanged reports that the worktree changed while prism copied it.
var errContextChanged = errors.New("the build context changed while prism copied it: try again")

// stagedBuild is the copy of one build.
type stagedBuild struct {
	dir     string
	context string
	file    string
	notes   []string
}

func (s *stagedBuild) remove() {
	if s != nil && s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
}

// resolveInWorktree resolves rel against the worktree with every symlink
// followed, and refuses a result outside the worktree. worktree must hold
// no symlink (Caller.validateWorktree).
func resolveInWorktree(worktree, flag, rel string) (string, error) {
	real, err := filepath.EvalSymlinks(filepath.Join(worktree, rel))
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s %q does not exist in the worktree", flag, rel)
	}
	if err != nil {
		return "", fmt.Errorf("%s %q: %v", flag, rel, err)
	}
	r, err := filepath.Rel(worktree, real)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s %q resolves outside the worktree", flag, rel)
	}
	return r, nil
}

// buildPaths are the context and the Containerfile of one build, as the
// agent gave them, relative to the worktree.
type buildPaths struct {
	context string
	file    string
}

// resolveBuildPaths checks the context and the Containerfile of v against
// the worktree. With no --file, it selects Containerfile, then Dockerfile,
// in the context directory, as podman does.
func resolveBuildPaths(worktree string, v validBuild) (buildPaths, error) {
	ctxReal, err := resolveInWorktree(worktree, "CONTEXT", v.context)
	if err != nil {
		return buildPaths{}, err
	}
	if info, err := os.Stat(filepath.Join(worktree, ctxReal)); err != nil || !info.IsDir() {
		return buildPaths{}, fmt.Errorf("CONTEXT %q is not a directory", v.context)
	}
	p := buildPaths{context: v.context, file: v.file}
	if p.file == "" {
		for _, name := range []string{"Containerfile", "Dockerfile"} {
			candidate := filepath.Join(v.context, name)
			if _, err := os.Stat(filepath.Join(worktree, candidate)); err == nil {
				p.file = candidate
				break
			}
		}
		if p.file == "" {
			return buildPaths{}, fmt.Errorf("CONTEXT %q holds no Containerfile or Dockerfile: add one, or use --file", v.context)
		}
	}
	fileReal, err := resolveInWorktree(worktree, "--file", p.file)
	if err != nil {
		return buildPaths{}, err
	}
	if info, err := os.Stat(filepath.Join(worktree, fileReal)); err != nil || !info.Mode().IsRegular() {
		return buildPaths{}, fmt.Errorf("--file %q is not a regular file", p.file)
	}
	return p, nil
}

// stageBuild copies the Containerfile and the context of p from the
// worktree into the directory name in the prism-container state tree. The
// caller removes the copy.
func stageBuild(ctx context.Context, worktree string, p buildPaths, name string) (*stagedBuild, error) {
	parent, err := container.PrismContainerBuildStageDirPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("create the build copy dir: %w", err)
	}
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create the build copy dir: %w", err)
	}
	s := &stagedBuild{
		dir:     dir,
		context: filepath.Join(dir, "context"),
		file:    filepath.Join(dir, "containerfile", filepath.Base(p.file)),
	}
	ok := false
	defer func() {
		if !ok {
			s.remove()
		}
	}()

	root, err := os.OpenRoot(worktree)
	if err != nil {
		return nil, fmt.Errorf("open the worktree: %w", err)
	}
	defer root.Close()

	if err := os.Mkdir(filepath.Dir(s.file), 0o700); err != nil {
		return nil, err
	}
	if err := copySmallFile(root, p.file, s.file); err != nil {
		return nil, fmt.Errorf("copy the Containerfile %q: %w", p.file, err)
	}
	// Podman reads <Containerfile>.containerignore or .dockerignore before
	// the ignore file of the context. Copy them next to the copy of the
	// Containerfile, so that podman makes the same choice.
	perFileIgnore := false
	for _, suffix := range []string{".containerignore", ".dockerignore"} {
		copied, err := copyIfRegular(root, p.file+suffix, s.file+suffix)
		if err != nil {
			return nil, fmt.Errorf("copy %q: %w", p.file+suffix, err)
		}
		perFileIgnore = perFileIgnore || copied
	}

	c := &contextCopier{ctx: ctx, root: root, base: filepath.ToSlash(p.context), dst: s.context, budget: MaxContextBytes}
	if !perFileIgnore {
		c.skip = readContextIgnore(root, p.context)
	}
	if err := c.copy(); err != nil {
		return nil, err
	}
	s.notes = c.notes
	ok = true
	return s, nil
}

// openRegular opens rel in root for reading and confirms that it is a
// regular file. O_NONBLOCK stops the open from blocking on a FIFO that the
// agent puts at the path.
func openRegular(root *os.Root, rel string) (*os.File, fs.FileInfo, error) {
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s is not a regular file", rel)
	}
	return f, info, nil
}

// copyData copies at most limit bytes from src to a new file at dst, and
// returns the number of bytes. A source longer than limit is an error.
func copyData(src io.Reader, dst string, perm fs.FileMode, limit int64) (int64, error) {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(src, limit+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, errContextTooLarge
	}
	return n, os.Chmod(dst, perm)
}

var errContextTooLarge = fmt.Errorf("the build context holds more than %d GiB of file data: make CONTEXT smaller, or exclude files in .containerignore", MaxContextBytes>>30)

func copySmallFile(root *os.Root, rel, dst string) error {
	f, info, err := openRegular(root, rel)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := copyData(f, dst, info.Mode().Perm(), maxSmallFileBytes); err != nil {
		if errors.Is(err, errContextTooLarge) {
			return fmt.Errorf("it is larger than %d MiB", maxSmallFileBytes>>20)
		}
		return err
	}
	return nil
}

// copyIfRegular copies rel when it is a regular file, and reports whether
// it did. A missing file is not an error.
func copyIfRegular(root *os.Root, rel, dst string) (bool, error) {
	info, err := root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	return true, copySmallFile(root, rel, dst)
}

// ignoreSubset holds the patterns of an ignore file that the copy applies
// itself. Podman applies the whole file to the copy again, so the copy
// must never leave out a path that podman keeps. It applies only the
// patterns whose podman meaning is plain: a literal path, and **/ followed
// by a literal path. Podman excludes such a path and everything below it.
// A file with a "!" exception applies no pattern, because an exception can
// keep a path below an excluded directory.
type ignoreSubset struct {
	exact    map[string]bool
	anyDepth map[string]bool
}

func (s ignoreSubset) match(rel string) bool {
	// Podman reads the ignore file from the copy. An ignore file that
	// lists itself must still reach the copy.
	if rel == ".containerignore" || rel == ".dockerignore" {
		return false
	}
	if s.exact[rel] {
		return true
	}
	if len(s.anyDepth) == 0 {
		return false
	}
	for suffix := range s.anyDepth {
		if rel == suffix || strings.HasSuffix(rel, "/"+suffix) {
			return true
		}
	}
	return false
}

// parseIgnoreSubset parses an ignore file as podman does (imagebuilder
// ParseIgnoreReader, then moby patternmatcher.New) and keeps the subset
// that ignoreSubset describes.
func parseIgnoreSubset(data []byte) ignoreSubset {
	s := ignoreSubset{exact: map[string]bool{}, anyDepth: map[string]bool{}}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		p := strings.TrimSpace(strings.Trim(line, "/"))
		if p == "" {
			continue
		}
		if p[0] == '!' {
			return ignoreSubset{}
		}
		p = path.Clean(p)
		if rest, ok := strings.CutPrefix(p, "**/"); ok {
			if isLiteralPattern(rest) {
				s.anyDepth[rest] = true
			}
			continue
		}
		if isLiteralPattern(p) {
			s.exact[p] = true
		}
	}
	return s
}

func isLiteralPattern(p string) bool {
	return p != "" && p != "." && !strings.HasPrefix(p, "../") && p != ".." && !strings.ContainsAny(p, `*?[]\`)
}

// readContextIgnore reads the ignore file of the context as podman selects
// it: .containerignore, then .dockerignore. A file that is not a regular
// file gives an empty subset, so the copy keeps every path.
func readContextIgnore(root *os.Root, contextRel string) ignoreSubset {
	for _, name := range []string{".containerignore", ".dockerignore"} {
		rel := filepath.Join(contextRel, name)
		info, err := root.Lstat(rel)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return ignoreSubset{}
		}
		f, _, err := openRegular(root, rel)
		if err != nil {
			return ignoreSubset{}
		}
		data, err := io.ReadAll(io.LimitReader(f, maxSmallFileBytes))
		_ = f.Close()
		if err != nil {
			return ignoreSubset{}
		}
		return parseIgnoreSubset(data)
	}
	return ignoreSubset{}
}

// contextCopier copies one context directory from root into dst.
type contextCopier struct {
	ctx    context.Context
	root   *os.Root
	base   string
	dst    string
	skip   ignoreSubset
	budget int64
	links  []string
	notes  []string
}

func (c *contextCopier) copy() error {
	err := fs.WalkDir(c.root.FS(), c.base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := c.ctx.Err(); err != nil {
			return err
		}
		rel := "."
		if p != c.base {
			if c.base == "." {
				rel = p
			} else {
				rel = strings.TrimPrefix(p, c.base+"/")
			}
			if c.skip.match(rel) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		dst := filepath.Join(c.dst, filepath.FromSlash(rel))
		switch {
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			// The owner keeps write access, so that the copy can be
			// filled and removed.
			if err := os.Mkdir(dst, 0o700); err != nil {
				return err
			}
			return os.Chmod(dst, info.Mode().Perm()|0o700)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := c.root.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, dst); err != nil {
				return err
			}
			c.links = append(c.links, rel)
			return nil
		case d.Type().IsRegular():
			return c.copyFile(p, dst)
		default:
			c.notes = append(c.notes, fmt.Sprintf("prism did not copy %s into the build context: it is not a regular file, a directory, or a symlink", rel))
			return nil
		}
	})
	if err != nil {
		if errors.Is(err, errContextTooLarge) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w (%v)", errContextChanged, err)
	}
	return c.dropEscapingLinks()
}

func (c *contextCopier) copyFile(p, dst string) error {
	f, info, err := openRegular(c.root, p)
	if err != nil {
		return err
	}
	defer f.Close()
	if info.Size() > c.budget {
		return errContextTooLarge
	}
	n, err := copyData(f, dst, info.Mode().Perm(), c.budget)
	if err != nil {
		return err
	}
	c.budget -= n
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

// dropEscapingLinks removes each copied symlink that resolves outside the
// copied context. The copy is not open to the agent, so the result holds
// for the whole build. A link to a missing path inside the context stays:
// it resolves to nothing.
func (c *contextCopier) dropEscapingLinks() error {
	if len(c.links) == 0 {
		return nil
	}
	staged, err := os.OpenRoot(c.dst)
	if err != nil {
		return err
	}
	defer staged.Close()
	for _, rel := range c.links {
		if _, err := staged.Stat(rel); err == nil || errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := os.Remove(filepath.Join(c.dst, filepath.FromSlash(rel))); err != nil {
			return err
		}
		c.notes = append(c.notes, fmt.Sprintf("prism did not copy the symlink %s into the build context: it resolves outside the build context", rel))
	}
	return nil
}
