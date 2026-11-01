module github.com/bazel-contrib/rules_img/img_tool

go 1.26.5

require (
	cloud.google.com/go/longrunning v1.4.1
	github.com/aws/aws-sdk-go-v2 v1.47.5
	github.com/aws/aws-sdk-go-v2/config v1.33.13
	github.com/aws/aws-sdk-go-v2/service/s3 v1.114.6
	github.com/awslabs/amazon-ecr-credential-helper/ecr-login v0.12.0
	github.com/bazelbuild/rules_go v0.65.0
	github.com/containerd/containerd/api v1.12.0
	github.com/containerd/platforms v1.0.0-rc.5
	github.com/containerd/stargz-snapshotter/estargz v0.19.1
	github.com/docker/cli v29.9.2+incompatible
	github.com/goccy/go-yaml v1.19.2
	github.com/google/go-containerregistry v0.22.1
	github.com/google/uuid v1.6.0
	github.com/jedib0t/go-pretty/v6 v6.8.3
	github.com/klauspost/compress v1.20.3
	github.com/klauspost/pgzip v1.3.0
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.1.1
	github.com/prometheus/client_golang v1.25.0
	github.com/sirupsen/logrus v1.10.2
	github.com/vbatts/go-mtree v0.7.0
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.47.0
	go.opentelemetry.io/otel/exporters/prometheus v0.69.0
	go.opentelemetry.io/otel/exporters/stdout/stdoutmetric v1.47.0
	go.opentelemetry.io/otel/metric v1.47.0
	go.opentelemetry.io/otel/sdk v1.47.0
	go.opentelemetry.io/otel/sdk/metric v1.47.0
	go.podman.io/storage v1.65.1-0.20261027184613-8756cda378b1
	golang.org/x/sync v0.24.0
	golang.org/x/term v0.47.0
	google.golang.org/genproto/googleapis/api v0.0.0-20261026165753-2ffab32bce7e
	google.golang.org/genproto/googleapis/bytestream v0.0.0-20261026165753-2ffab32bce7e
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261026165753-2ffab32bce7e
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
)

require (
	cloud.google.com/go/compute/metadata v0.10.0 // indirect
	cyphar.com/go-pathrs v0.2.6 // indirect
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.25 // indirect
	github.com/aws/aws-sdk-go-v2/credentials v1.20.13 // indirect
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.20.5 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.8 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.8 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/ecr v1.67.2 // indirect
	github.com/aws/aws-sdk-go-v2/service/ecrpublic v1.47.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.23 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.9 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.8 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.12.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.39.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.44.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/sts v1.51.6 // indirect
	github.com/aws/smithy-go v1.28.7 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/containerd/log v0.2.0 // indirect
	github.com/containerd/ttrpc v1.2.10 // indirect
	github.com/cyphar/filepath-securejoin v0.7.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/docker/docker-credential-helpers v0.9.12 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.33.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/mattn/go-runewidth v0.0.31 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/moby/sys/capability v0.5.0 // indirect
	github.com/moby/sys/mountinfo v0.7.3 // indirect
	github.com/moby/sys/user v0.5.0 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.3-0.20250322232337-35a7c28c31ee // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/opencontainers/runtime-spec v1.3.0 // indirect
	github.com/pandatix/go-abnf v0.5.5 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.73.1 // indirect
	github.com/prometheus/otlptranslator v1.0.0 // indirect
	github.com/prometheus/procfs v0.23.0 // indirect
	github.com/ulikunitz/xz v0.5.17 // indirect
	github.com/vbatts/tar-split v0.12.3 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.1 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	gotest.tools/v3 v3.5.2 // indirect
)
