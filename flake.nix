{
  description = "rules_img";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    bazel-env.url = "github:malt3/bazel-env";
    bazel-env.inputs.nixpkgs.follows = "nixpkgs";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { nixpkgs, flake-utils, bazel-env, ... }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs {
          inherit system;
        };
        isLinux = pkgs.stdenv.isLinux;
        bazel_pkgs = bazel-env.packages.${system};
        # Common packages for all platforms
        commonPkgs = with pkgs; [
          pre-commit
        ];
      in
      if isLinux then
        rec {
          packages.dev = (bazel_pkgs.bazel-full-env.override {
            name = "dev";
            extraPkgs = commonPkgs;
          });
          packages.bazel-fhs = bazel_pkgs.bazel-full;
          devShells.dev = packages.dev.env;
          devShells.default = pkgs.mkShell {
            packages = [ packages.dev bazel_pkgs.bazel-full ] ++ commonPkgs;
          };
        }
      else
        {
          # macOS: simpler dev shell without Bazel
          # Use Bazelisk instead.
          devShells.default = pkgs.mkShell {
            packages = commonPkgs;
          };
        });
}
