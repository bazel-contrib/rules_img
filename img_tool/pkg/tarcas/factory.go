package tarcas

import (
	"crypto/sha256"
	"errors"
	"hash"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/digestfs"
)

type SHA256Helper struct{}

func (SHA256Helper) New() hash.Hash {
	return sha256.New()
}

func NewSHA256CAS(appender api.TarAppender, options ...Option) *CAS[SHA256Helper] {
	return New[SHA256Helper](appender, options...)
}

func NewSHA256CASWithDigestFS(appender api.TarAppender, digestFS *digestfs.FileSystem, options ...Option) *CAS[SHA256Helper] {
	return NewWithDigestFS[SHA256Helper](appender, digestFS, options...)
}

func CASFactory(hashAlgorithm string, appender api.TarAppender, options ...Option) (api.TarCAS, error) {
	switch hashAlgorithm {
	case "sha256":
		return NewSHA256CAS(appender, options...), nil
	}
	return nil, errors.New("unsupported hash algorithm")
}

func CASFactoryWithDigestFS(hashAlgorithm string, appender api.TarAppender, digestFS *digestfs.FileSystem, options ...Option) (api.TarCAS, error) {
	switch hashAlgorithm {
	case "sha256":
		return NewSHA256CASWithDigestFS(appender, digestFS, options...), nil
	}
	return nil, errors.New("unsupported hash algorithm")
}
