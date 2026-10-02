package mirror

import (
	"bytes"
	"context"
	"errors"
	"hash"
	"io"
	"io/fs"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/inode64/fsledger/internal/fault"
	"github.com/inode64/fsledger/internal/integrity"
	"github.com/inode64/fsledger/internal/pathutil"
	"github.com/inode64/fsledger/internal/resource"
)

type copyMode uint8

const (
	copyCached copyMode = iota
	copyEvent
	copyForced
)

// racyWindow covers the coarsest timestamp granularity in use (FAT: two seconds).
const racyWindow = 2 * time.Second

type cachedFile struct {
	source      signature
	destination signature
	// verified is when the source bytes were last known to match the mirror.
	verified int64
}

// racy mirrors Git's racy timestamps: a rewrite of equal size within the same
// timestamp tick as the cached ctime leaves the signature intact, so metadata
// proves nothing until the content was verified clearly after that tick.
func (file cachedFile) racy() bool {
	return max(file.source.ctime, file.source.mtime) >= file.verified-int64(racyWindow)
}

func (s *Sync) destinationSignature(relative string) (signature, error) {
	info, err := s.root.Lstat(relative)
	if err != nil {
		return signature{}, fault.Wrap("stat mirror cache", err)
	}

	return metadataSignature(info)
}

func metadataSignature(info fs.FileInfo) (signature, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return signature{}, fault.New("mirror metadata lacks Linux stat")
	}

	return signature{
		size: stat.Size, mtime: stat.Mtim.Nano(), ctime: stat.Ctim.Nano(), inode: stat.Ino,
		device: stat.Dev, mode: fs.FileMode(stat.Mode),
	}, nil
}

// chunkBytes bounds the work between cancellation checks while copying or comparing.
const chunkBytes = 1 << 20

// sameContents compares the mirror copy with the source chunk by chunk, hashing the source on the way
// so that a kept copy still offers its digest to the catalog. An explicit event must still check
// bytes: matching metadata is not proof that content is unchanged. A missing copy, or one whose
// type, size or mode differ, is not the same, and rewriting it is the answer rather than an error.
func (s *Sync) sameContents(ctx context.Context, source *os.File, relative string, sig signature) (
	bool, hash.Hash, error,
) {
	// os.Root resolves in-root symlinks even with O_NOFOLLOW, so only an lstat proves a regular copy.
	info, err := s.root.Lstat(relative)
	if replaceable(err) {
		return false, nil, nil
	}

	if err != nil {
		return false, nil, fault.Wrap("stat mirror copy", err)
	}

	if !info.Mode().IsRegular() || info.Size() != sig.size || info.Mode().Perm() != mirrorMode(sig.mode) {
		return false, nil, nil
	}

	destination, err := s.root.OpenFile(relative, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return false, nil, fault.Wrap("open mirror copy", err)
	}
	defer resource.Close(destination)

	opened, err := destination.Stat()
	if err != nil {
		return false, nil, fault.Wrap("stat mirror copy", err)
	}

	if !os.SameFile(info, opened) {
		return false, nil, nil
	}

	digest, err := s.newDigest()
	if err != nil {
		return false, nil, err
	}

	input := io.LimitReader(source, sig.size)
	if digest != nil {
		input = io.TeeReader(input, digest)
	}

	equal, err := s.equalContent(ctx, input, destination, sig.size)

	return equal, digest, err
}

// replaceable reports a mirror entry that cannot be a regular copy: absent or below a non-directory.
func replaceable(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR)
}

// equalContent stops at the first difference. A short read means one side changed size; the caller
// then finds a changed source unstable or rewrites a changed mirror copy.
func (s *Sync) equalContent(ctx context.Context, source, mirror io.Reader, size int64) (bool, error) {
	const sides = 2

	chunk := min(size, chunkBytes)
	if int64(cap(s.compareBuffer)) < sides*chunk {
		s.compareBuffer = make([]byte, sides*chunk)
	}

	left, right := s.compareBuffer[:chunk], s.compareBuffer[chunk:sides*chunk]

	for remaining := size; remaining > 0; {
		err := ctx.Err()
		if err != nil {
			return false, fault.Wrap("comparison cancelled", err)
		}

		length := min(remaining, chunkBytes)

		_, err = io.ReadFull(source, left[:length])
		if err == nil {
			_, err = io.ReadFull(mirror, right[:length])
		}

		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}

		if err != nil {
			return false, fault.Wrap("compare mirror copy", err)
		}

		if !bytes.Equal(left[:length], right[:length]) {
			return false, nil
		}

		remaining -= length
	}

	return true, nil
}

// newDigest returns nil when the catalog asked for no copied hashes.
func (s *Sync) newDigest() (hash.Hash, error) {
	if s.algorithm == "" {
		return nil, nil //nolint:nilnil // No algorithm means no digest, not a failure.
	}

	digest, err := integrity.Hasher(s.algorithm)

	return digest, fault.Wrap("mirror digest", err)
}

// CopyN retains file-to-file acceleration while bounding work between cancellation checks.
func copyContent(ctx context.Context, destination io.Writer, source io.Reader) error {
	for {
		err := ctx.Err()
		if err != nil {
			return fault.Wrap("copy cancelled", err)
		}

		_, err = io.CopyN(destination, source, chunkBytes)
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fault.Wrap("copy content", err)
		}
	}
}

type copiedFile struct {
	digest    string
	signature signature
	hashedAt  int64
}

// CopiedHash never returns a digest from a prior mirror operation. The scanner
// verifies source stability again after reading attributes and obtaining this hash.
func (s *Sync) CopiedHash(record integrity.Record, algorithm string) (string, int64, bool) {
	value, exists := s.hashes[string(record.Path)]

	expected := signature{
		size:   record.Size,
		mtime:  record.Mtime,
		ctime:  record.Ctime,
		inode:  record.Inode,
		device: record.Device,
		mode:   fs.FileMode(record.Mode),
	}
	if !exists || s.algorithm != algorithm || value.signature != expected {
		return "", 0, false
	}

	return value.digest, value.hashedAt, true
}

// ApplyPaths checks configured roots once around one serialized event group.
func (s *Sync) ApplyPaths(ctx context.Context, paths []string) error {
	clear(s.hashes)

	err := s.roots.Check(s.sources)
	if err != nil {
		return err
	}

	var failure error

	for _, path := range pathutil.CompactRoots(paths) {
		err := s.apply(ctx, path, copyEvent)
		failure = errors.Join(failure, err)

		_, fatal := integrity.SplitUnstable(err)
		if fatal != nil {
			return failure
		}
	}

	return errors.Join(failure, s.roots.Check(s.sources))
}
