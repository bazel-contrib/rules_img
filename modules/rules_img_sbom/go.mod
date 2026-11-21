module github.com/bazel-contrib/rules_img_sbom

go 1.27.0

toolchain go1.27.3

require (
	github.com/CycloneDX/cyclonedx-go v0.12.0
	github.com/bazel-contrib/supply-chain/lib/supplychain-go v0.0.0-20261119132701-c18a7daa7a4e
	github.com/google/uuid v1.6.0
	github.com/package-url/packageurl-go v0.1.7
	github.com/spdx/tools-golang v0.6.0-rc4
	github.com/stretchr/testify v1.12.1
)

require (
	github.com/anchore/go-struct-converter v0.2.0-rc2 // indirect
	github.com/piprate/json-gold v0.7.0 // indirect
	github.com/pquerna/cachecontrol v0.0.0-20180517163645-1555304b9b35 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
)
