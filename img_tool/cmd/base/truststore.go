package base

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/basemeta"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/basemeta/pkgfile"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/basemeta/truststore"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/baselayer"
)

// certificatePaths are the locations distribution packages keep their CA
// certificates in. Only files under these prefixes are harvested from a .deb or
// .rpm; nothing else about the package is read.
var certificatePaths = pkgfile.PrefixMatcher(
	// Debian / Ubuntu: ca-certificates ships PEM files here, and the
	// concatenated bundle is generated into /etc/ssl/certs.
	"usr/share/ca-certificates",
	"etc/ssl/certs",
	// Fedora / RHEL / SUSE: ca-certificates ships extracted bundles here.
	"usr/share/pki/ca-trust-source",
	"etc/pki/ca-trust/source",
	"etc/pki/tls/certs",
	"var/lib/ca-certificates",
)

// trustStoreProcess implements `img base trust-store`.
func trustStoreProcess(_ context.Context, args []string) {
	var certs, debs, rpms stringsFlag
	var outputPath, producer string
	var bundlePath, explodedDir, javaKeystorePath, javaKeystorePassword string
	var writeBundle, writeExploded, writeJavaKeystore bool
	var mode modeFlag

	flagSet := flag.NewFlagSet("base trust-store", flag.ExitOnError)
	flagSet.Var(&certs, "cert", "Path of a certificate file (PEM, DER or PKCS#7). Can be repeated.")
	flagSet.Var(&debs, "deb", "Path of a .deb package to harvest certificates from. Can be repeated.")
	flagSet.Var(&rpms, "rpm", "Path of an .rpm package to harvest certificates from. Can be repeated.")
	flagSet.StringVar(&outputPath, "output", "", "Path of the base metadata stream to write.")
	flagSet.StringVar(&producer, "producer", "", "Label of the rule producing this stream, used in conflict messages.")
	flagSet.BoolVar(&writeBundle, "bundle", true, "Write a single concatenated PEM bundle.")
	flagSet.StringVar(&bundlePath, "bundle-path", "/etc/ssl/certs/ca-certificates.crt", "Path of the PEM bundle inside the image.")
	flagSet.BoolVar(&writeExploded, "exploded", true, "Write an exploded certificate directory with subject-hash links.")
	flagSet.StringVar(&explodedDir, "exploded-dir", "/etc/ssl/certs", "Directory of the exploded certificate tree inside the image.")
	flagSet.BoolVar(&writeJavaKeystore, "java-keystore", false, "Write a PKCS#12 truststore for the JVM.")
	flagSet.StringVar(&javaKeystorePath, "java-keystore-path", "/etc/ssl/certs/java/cacerts", "Path of the PKCS#12 truststore inside the image.")
	flagSet.StringVar(&javaKeystorePassword, "java-keystore-password", "changeit", "Password protecting the truststore's integrity check.")
	flagSet.Var(&mode, "mode", "Octal file mode of the written certificate files. Defaults to 0644.")
	if err := flagSet.Parse(args); err != nil {
		fail("trust-store", err)
	}

	collection := truststore.NewCollection()
	for _, certPath := range certs {
		data, err := os.ReadFile(certPath)
		if err != nil {
			fail("trust-store", fmt.Errorf("reading certificate: %w", err))
		}
		if err := collection.AddBytes(data, certPath); err != nil {
			fail("trust-store", err)
		}
	}
	for _, debPath := range debs {
		if err := addPackageCertificates(collection, debPath, pkgfile.ExtractDeb); err != nil {
			fail("trust-store", err)
		}
	}
	for _, rpmPath := range rpms {
		if err := addPackageCertificates(collection, rpmPath, pkgfile.ExtractRPM); err != nil {
			fail("trust-store", err)
		}
	}

	if collection.Len() == 0 {
		fail("trust-store", fmt.Errorf("no certificates found in any input"))
	}
	collected := collection.Certificates()

	fileMode := mode.or(0o644)
	var entries []*baselayer.BaseEntry

	if writeBundle {
		bundle, err := truststore.Bundle(collected)
		if err != nil {
			fail("trust-store", err)
		}
		entries = append(entries, basemeta.File(bundlePath, fileMode, bundle))
	}

	if writeExploded {
		exploded, err := truststore.Exploded(collected)
		if err != nil {
			fail("trust-store", err)
		}
		for _, entry := range exploded {
			imagePath := path.Join(explodedDir, entry.Name)
			if entry.LinkTarget != "" {
				// The links are relative to the directory they live in, so the
				// tree keeps resolving wherever it is mounted.
				entries = append(entries, basemeta.Symlink(imagePath, entry.LinkTarget))
				continue
			}
			entries = append(entries, basemeta.File(imagePath, fileMode, entry.PEM))
		}
	}

	if writeJavaKeystore {
		keystore, err := truststore.WritePKCS12(collected, javaKeystorePassword)
		if err != nil {
			fail("trust-store", err)
		}
		entries = append(entries, basemeta.File(javaKeystorePath, fileMode, keystore))
	}

	if len(entries) == 0 {
		fail("trust-store", fmt.Errorf("nothing to write: enable at least one of --bundle, --exploded or --java-keystore"))
	}

	if err := writeStream(outputPath, producer, entries); err != nil {
		fail("trust-store", err)
	}
}

// addPackageCertificates harvests every certificate file at a well-known path
// inside a package and adds it to the collection.
func addPackageCertificates(collection *truststore.Collection, pkgPath string, extract func(string, pkgfile.Matcher) ([]pkgfile.Entry, error)) error {
	files, err := extract(pkgPath, certificatePaths)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("%s: no certificates found under any of the standard CA certificate directories", pkgPath)
	}
	for _, file := range files {
		origin := pkgPath + ":" + file.Path
		if err := collection.AddBytes(file.Content, origin); err != nil {
			return err
		}
	}
	return nil
}
