package cas

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeSource is a blobSource that records how many calls it handled.
type fakeSource struct {
	id int

	mu    sync.Mutex
	calls int
}

func (f *fakeSource) FindMissingBlobs(context.Context, []Digest) ([]Digest, error) {
	f.record()
	return nil, nil
}

func (f *fakeSource) ReadBlob(context.Context, Digest) ([]byte, error) {
	f.record()
	return nil, nil
}

func (f *fakeSource) ReaderForBlob(context.Context, Digest) (io.ReadCloser, error) {
	f.record()
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeSource) record() {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
}

func TestPoolPickRoundRobin(t *testing.T) {
	a, b, c := &fakeSource{id: 0}, &fakeSource{id: 1}, &fakeSource{id: 2}
	p := newPool([]blobSource{a, b, c})

	var got []int
	for range 7 {
		got = append(got, p.pick().(*fakeSource).id)
	}
	want := []int{0, 1, 2, 0, 1, 2, 0}
	if !slices.Equal(got, want) {
		t.Fatalf("pick order = %v, want %v", got, want)
	}
}

func TestPoolDistributesPublicMethods(t *testing.T) {
	members := []*fakeSource{{}, {}, {}}
	sources := make([]blobSource, len(members))
	for i, m := range members {
		sources[i] = m
	}
	p := newPool(sources)

	ctx := context.Background()
	// Each iteration issues one of each read method (3 picks); 3 iterations
	// spread 9 picks evenly across the 3 members.
	for range 3 {
		if _, err := p.FindMissingBlobs(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := p.ReadBlob(ctx, Digest{}); err != nil {
			t.Fatal(err)
		}
		rc, err := p.ReaderForBlob(ctx, Digest{})
		if err != nil {
			t.Fatal(err)
		}
		rc.Close()
	}

	for i, m := range members {
		if m.calls != 3 {
			t.Errorf("member %d handled %d calls, want 3", i, m.calls)
		}
	}
}

func TestPoolSingleMember(t *testing.T) {
	only := &fakeSource{}
	p := newPool([]blobSource{only})

	ctx := context.Background()
	for range 5 {
		if _, err := p.ReadBlob(ctx, Digest{}); err != nil {
			t.Fatal(err)
		}
	}
	if only.calls != 5 {
		t.Fatalf("single member handled %d calls, want 5", only.calls)
	}
}

func TestNewPoolEmptyPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for empty pool")
		}
	}()
	newPool(nil)
}
