package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/bazel-contrib/rules_img_sbom/internal/layerscan"
	"github.com/bazel-contrib/rules_img_sbom/internal/scan"
)

// ScanRequest is written by the toolchain's scan_layer (see //sbom:toolchain.bzl).
type ScanRequest struct {
	// Contents is the command that writes the layer's files to its standard
	// output, as an uncompressed tar (`img layer-tar`), or nil if the layer's
	// files are not available.
	Contents *struct {
		Tool      string   `json:"tool"`
		Arguments []string `json:"arguments"`
	} `json:"contents"`
	// Unavailable is the error to fail with when Contents is nil: the layer had to
	// be scanned.
	Unavailable string `json:"unavailable"`
	// Metadata is the path of the layer's metadata (its digest, ...), or "".
	Metadata  string `json:"metadata"`
	MediaType string `json:"media_type"`
}

func scanLayerMain(args []string) {
	flags := flag.NewFlagSet("scan-layer", flag.ExitOnError)
	requestPath := flags.String("request", "", "Path to the JSON request written by the toolchain")
	outPath := flags.String("out", "", "Path to write the scan result to")
	if err := flags.Parse(args); err != nil || *requestPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "Usage: img-sbom scan-layer --request <request.json> --out <result.json>")
		os.Exit(1)
	}
	if err := scanLayer(context.Background(), *requestPath, *outPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func scanLayer(ctx context.Context, requestPath, outPath string) error {
	var request ScanRequest
	if err := readJSON(requestPath, &request); err != nil {
		return fmt.Errorf("reading the request: %w", err)
	}
	if request.Contents == nil {
		if request.Unavailable != "" {
			return errors.New(request.Unavailable)
		}
		return errors.New("the layer's files are not available")
	}
	layer := layerscan.Layer{MediaType: request.MediaType}
	if request.Metadata != "" {
		var metadata struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			DiffID    string `json:"diff_id"`
		}
		if err := readJSON(request.Metadata, &metadata); err != nil {
			return fmt.Errorf("reading the layer metadata: %w", err)
		}
		layer.Digest, layer.DiffID = metadata.Digest, metadata.DiffID
		if metadata.MediaType != "" {
			layer.MediaType = metadata.MediaType
		}
	}

	cmd := exec.CommandContext(ctx, request.Contents.Tool, request.Contents.Arguments...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("reading the layer: %w", err)
	}
	result, scanErr := scan.Scan(ctx, stdout, scan.Options{Layer: layer})
	if scanErr != nil {
		// Unblock the tool if the scan stopped reading early.
		_ = cmd.Process.Kill()
	}
	if err := cmd.Wait(); err != nil && scanErr == nil {
		return fmt.Errorf("reading the layer with %s: %w", request.Contents.Tool, err)
	}
	if scanErr != nil {
		return scanErr
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, append(data, '\n'), 0o644)
}
