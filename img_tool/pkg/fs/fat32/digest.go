package fat32

// pathKeyOf returns the key under which the digest of the content of a file
// read from a path is remembered.
func pathKeyOf(n *node) (pathKey, bool) {
	p, ok := n.src.(pathSource)
	return pathKey{path: string(p), size: n.size}, ok
}

// knownPathDigest returns the remembered digest of a file read from a path.
func (b *Builder) knownPathDigest(n *node) ([]byte, bool) {
	key, ok := pathKeyOf(n)
	if !ok {
		return nil, false
	}
	d, ok := b.pathDigests[key]
	return d, ok
}

func (b *Builder) rememberPathDigest(n *node) {
	if key, ok := pathKeyOf(n); ok {
		b.pathDigests[key] = n.digest
	}
}
