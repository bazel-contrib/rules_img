package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

type platformBinaries []struct {
	os   string
	cpu  string
	path string
}

func (pb *platformBinaries) String() string {
	return fmt.Sprintf("%v", *pb)
}

func (pb *platformBinaries) Set(value string) error {
	kv := strings.SplitN(value, "=", 2)
	if len(kv) != 2 {
		return fmt.Errorf("invalid value: expected key value pair separated by =")
	}
	os_cpu := strings.SplitN(kv[0], "_", 2)
	if len(os_cpu) != 2 {
		return fmt.Errorf("invalid value: expected os and cpu separated by _")
	}
	*pb = append(*pb, struct {
		os   string
		cpu  string
		path string
	}{
		os:   os_cpu[0],
		cpu:  os_cpu[1],
		path: kv[1],
	})
	return nil
}

type lockfileItem struct {
	Version   string `json:"version"`
	Integrity string `json:"integrity"`
	OS        string `json:"os"`
	CPU       string `json:"cpu"`
}

var (
	version  string
	binaries platformBinaries
)

func main() {
	flag.StringVar(&version, "version", "0.0.0", "Version of the project.")
	flag.Var(&binaries, "tool", "Key-value pairs of platform name to tool binary path.")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "expected lockfile output")
		os.Exit(1)
	}
	var lockfileItems []lockfileItem
	for _, bin := range binaries {
		sri, err := fileSRI(bin.path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "calculating sri of %s: %v\n", bin.path, err)
			os.Exit(1)
		}
		lockfileItems = append(lockfileItems, lockfileItem{
			Version:   "v" + version,
			Integrity: sri,
			OS:        bin.os,
			CPU:       bin.cpu,
		})
	}
	lockfile, err := json.Marshal(lockfileItems)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshaling lockfile: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(flag.Arg(0), lockfile, os.ModePerm); err != nil {
		fmt.Fprintf(os.Stderr, "writing lockfile: %v\n", err)
		os.Exit(1)
	}
}

func fileSRI(source string) (string, error) {
	reader, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	hash := sha256.New()
	_, err = io.Copy(hash, reader)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256-%s", base64.StdEncoding.EncodeToString([]byte(hash.Sum(nil)))), nil
}
