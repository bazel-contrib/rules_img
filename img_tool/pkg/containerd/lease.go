package containerd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	api "github.com/containerd/containerd/api/services/leases/v1"
)

// ImageService is the image service interface
type LeaseService interface {
	Create(ctx context.Context, labels map[string]string) (string, error)
	Delete(ctx context.Context, id string) error
}

type leaseService struct {
	client api.LeasesClient
}

func generateLeaseID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("failed to read random bytes: %v", err))
	}
	return "lease-" + hex.EncodeToString(b)
}

// Create creates a new image
func (s *leaseService) Create(ctx context.Context, labels map[string]string) (string, error) {
	req := &api.CreateRequest{
		ID:     generateLeaseID(),
		Labels: labels,
	}

	resp, err := s.client.Create(ctx, req)
	if err != nil {
		return "", err
	}

	return resp.Lease.ID, nil
}

// Update updates an existing image
func (s *leaseService) Delete(ctx context.Context, lease string) error {
	req := &api.DeleteRequest{
		ID: lease,
	}

	_, err := s.client.Delete(ctx, req)
	if err != nil {
		return err
	}

	return nil
}
