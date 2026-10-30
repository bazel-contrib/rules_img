// Package casdirlayout defines the on-disk layout of the content-addressed
// input directory (.inputfilecas) built by `img cas-dir`.
//
// Blobs are stored at <sha256 dir>/<first two hex digits>/<hex>. Sharding keeps
// every directory small: as a Bazel tree artifact the directory is uploaded to
// remote execution as a single REAPI Directory message per directory, and one
// flat directory holding every input file of a large layer exceeds the message
// size limit of common servers (16 MiB on Buildbarn).
//
// Readers also accept the flat <sha256 dir>/<hex> layout, which is what OCI
// layouts use for blobs/sha256 and what older versions of `img cas-dir` wrote.
package casdirlayout

import (
	"os"
	"path/filepath"
)

// shardLen is the number of leading hex digits that name the shard directory.
const shardLen = 2

// ShardDir returns the directory that holds hexDigest below shaDir.
func ShardDir(shaDir, hexDigest string) string {
	if len(hexDigest) < shardLen {
		return shaDir
	}
	return filepath.Join(shaDir, hexDigest[:shardLen])
}

// Path returns where `img cas-dir` stores the blob with the given digest.
func Path(shaDir, hexDigest string) string {
	return filepath.Join(ShardDir(shaDir, hexDigest), hexDigest)
}

// Open opens the blob with the given digest, trying the sharded layout first
// and then the flat one.
func Open(shaDir, hexDigest string) (*os.File, error) {
	f, err := os.Open(Path(shaDir, hexDigest))
	if err == nil {
		return f, nil
	}
	if flat, flatErr := os.Open(filepath.Join(shaDir, hexDigest)); flatErr == nil {
		return flat, nil
	}
	return nil, err
}

// Stat is like Open but returns the file info of the blob.
func Stat(shaDir, hexDigest string) (os.FileInfo, error) {
	info, err := os.Stat(Path(shaDir, hexDigest))
	if err == nil {
		return info, nil
	}
	if flat, flatErr := os.Stat(filepath.Join(shaDir, hexDigest)); flatErr == nil {
		return flat, nil
	}
	return nil, err
}
