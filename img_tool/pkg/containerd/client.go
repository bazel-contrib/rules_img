package containerd

import (
	"context"
	"fmt"
	"net"

	contentapi "github.com/containerd/containerd/api/services/content/v1"
	imagesapi "github.com/containerd/containerd/api/services/images/v1"
	leasesapi "github.com/containerd/containerd/api/services/leases/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client is a minimal containerd client
type Client struct {
	conn          *grpc.ClientConn
	contentClient contentapi.ContentClient
	imagesClient  imagesapi.ImagesClient
	leasesClient  leasesapi.LeasesClient
	address       string
}

// New creates a new containerd client
func New(address string) (*Client, error) {
	if address == "" {
		addr, err := FindContainerdSocket()
		if err != nil {
			return nil, fmt.Errorf("finding containerd socket: %w", err)
		}
		address = addr
	}

	dialer := func(ctx context.Context, addr string) (net.Conn, error) {
		return net.Dial("unix", addr)
	}

	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
		grpc.WithUnaryInterceptor(namespaceInterceptor),
		grpc.WithStreamInterceptor(namespaceStreamInterceptor),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to containerd: %w", err)
	}

	return &Client{
		conn:          conn,
		contentClient: contentapi.NewContentClient(conn),
		imagesClient:  imagesapi.NewImagesClient(conn),
		leasesClient:  leasesapi.NewLeasesClient(conn),
		address:       address,
	}, nil
}

// Close closes the client connection
func (c *Client) Close() error {
	return c.conn.Close()
}

// ContentStore returns the content store
func (c *Client) ContentStore() Store {
	return &contentStore{client: c.contentClient}
}

func (c *Client) LeaseService() LeaseService {
	return &leaseService{client: c.leasesClient}
}

// ImageService returns the image service
func (c *Client) ImageService() ImageService {
	return &imageService{client: c.imagesClient}
}
