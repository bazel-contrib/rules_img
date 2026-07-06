// Package version holds the rules_img version, stamped in at build time.
package version

// Version is the rules_img version. It defaults to "dev" and is overridden
// via x_defs on the img go_binary target with the version of the rules_img
// Bazel module.
var Version = "dev"
