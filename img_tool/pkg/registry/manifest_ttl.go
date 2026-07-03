// Copyright 2026 The rules_img Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package registry

import "time"

// Manifest TTL state is intentionally kept beside the existing in-memory
// manifest map rather than inside manifest values. A single push creates two
// independently addressable references, the mutable target requested by the
// client and the immutable digest calculated by the registry. Keeping expiry by
// reference lets a rewritten tag outlive the older digest it replaced while
// still evicting both references for ordinary one-tag pushes.
func (m *manifests) evictExpired() {
	if m.ttl <= 0 {
		return
	}

	m.lock.Lock()
	defer m.lock.Unlock()
	m.evictExpiredLocked()
}

func (m *manifests) evictExpiredLocked() {
	now := m.now()
	for repo, refs := range m.expires {
		for ref, expiresAt := range refs {
			if expiresAt.After(now) {
				continue
			}
			delete(m.manifests[repo], ref)
			delete(refs, ref)
		}
		m.deleteEmptyRepoLocked(repo)
	}
}

func (m *manifests) newExpiry() time.Time {
	if m.ttl <= 0 {
		return time.Time{}
	}
	return m.now().Add(m.ttl)
}

func (m *manifests) setExpiryLocked(repo, target string, expiresAt time.Time) {
	if expiresAt.IsZero() {
		return
	}
	if _, ok := m.expires[repo]; !ok {
		m.expires[repo] = make(map[string]time.Time, 2)
	}
	m.expires[repo][target] = expiresAt
}

func (m *manifests) deleteExpiryLocked(repo, target string) {
	if m.ttl <= 0 {
		return
	}
	if refs, ok := m.expires[repo]; ok {
		delete(refs, target)
	}
	m.deleteEmptyRepoLocked(repo)
}

func (m *manifests) deleteEmptyRepoLocked(repo string) {
	if refs, ok := m.manifests[repo]; ok && len(refs) == 0 {
		delete(m.manifests, repo)
	}
	if refs, ok := m.expires[repo]; ok && len(refs) == 0 {
		delete(m.expires, repo)
	}
}

func defaultManifestClock() time.Time {
	return time.Now()
}
