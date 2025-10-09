# Custom Distroless Base Image Example

This example demonstrates how to use [rules_distroless](https://github.com/bazel-contrib/rules_distroless) with rules_img to create custom container images with minimal OS distributions.

## Features Demonstrated

This example showcases all major features of rules_distroless:

### 1. User and Group Management
- **`group` rule**: Creates `/etc/group` entries (similar to `groupadd`)
- **`passwd` rule**: Creates `/etc/passwd` entries (similar to `useradd`)
- Defines multiple users and groups including root, appuser, database users, and www-data

### 2. Certificate Management
- **`cacerts` rule**: Generates CA certificate bundles
- **`java_keystore` rule**: Creates Java keystores with custom certificates
- Example certificates included: Amazon Root CA 1 and 2

### 3. Package Management
- **`apt.yaml`**: Defines packages to install from Debian repositories
- **`apt.lock.json`**: Lock file for reproducible package versions
- **`dpkg_status` rule**: Creates package database for vulnerability scanners
