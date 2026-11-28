package bom

import (
	"fmt"
	"strings"

	"github.com/bazel-contrib/rules_img_sbom/internal/layerscan"
)

// Layer is a layer of an image manifest, as the SBOM sees it.
type Layer struct {
	// Scan is the path of the layer's scan result (see layerscan), or "" if the
	// layer wasn't scanned.
	Scan string `json:"scan"`
	// Base says whether the layer is one of the base image's.
	Base bool `json:"base"`
}

// addLayerScans adds the packages the image's layers (bottom to top) were
// found to contain. A package found by files that a layer above replaces or
// deletes is left out: it was removed or replaced, and the scan of the layer
// that did so found what replaced it (an upgraded package's database lists the
// new version), unless that scan found the same package again; and so is a
// package of the base image's SBOM that was found that way. The packages of the base image's layers are part of the base
// image's component (baseRef), if there is one, and the others of the image's
// (primary). Packages that are already in the document (from Bazel package
// metadata or the base image's SBOM) are merged into those.
//
// Operating system packages whose package URL names a distribution get the
// distribution of the image: the topmost layer that names one, else
// fallbackDistro.
func addLayerScans(b *Builder, layers []Layer, primary, baseRef string, inherited map[string]bool, fallbackDistro *layerscan.Distro) error {
	type scanned struct {
		result *layerscan.Result
		base   bool
	}
	var scans []scanned
	for _, layer := range layers {
		if layer.Scan == "" {
			continue
		}
		result, err := layerscan.Read(layer.Scan)
		if err != nil {
			return fmt.Errorf("reading the layer scan %s: %w", layer.Scan, err)
		}
		if result.Version != layerscan.Version {
			return fmt.Errorf("the layer scan %s has version %d, expected %d", layer.Scan, result.Version, layerscan.Version)
		}
		scans = append(scans, scanned{result: result, base: layer.Base})
	}

	distro, distroInBase := fallbackDistro, true
	for i := len(scans) - 1; i >= 0; i-- {
		if scans[i].result.Distro != nil {
			distro, distroInBase = scans[i].result.Distro, scans[i].base
			break
		}
	}
	parentOf := func(base bool) string {
		if base && baseRef != "" {
			return baseRef
		}
		return primary
	}
	if distro != nil && distro.ID != "" && distro != fallbackDistro {
		ref := b.Merge(operatingSystemComponent(distro))
		b.Depend(parentOf(distroInBase), ref)
	}

	shadow := layerscan.NewShadow()
	found := map[string]bool{}
	stale := map[string]bool{}
	for i := len(scans) - 1; i >= 0; i-- {
		scan := scans[i]
		for _, p := range scan.result.Packages {
			c := scannedComponent(p, distro, scan.result.Layer.Digest)
			if !hidden(shadow, p.Locations) {
				ref := b.Merge(c)
				found[ref] = true
				b.Depend(parentOf(scan.base), ref)
				continue
			}
			ref := b.Find(c)
			switch {
			case ref == "":
			case found[ref]:
				// Still there: a layer above found the same package again
				// (rewriting the package database for another). It came with
				// this layer.
				b.Depend(parentOf(scan.base), ref)
			case inherited[ref]:
				// The base image's SBOM describes a package that a layer above
				// the base image removed or replaced (an upgrade).
				stale[ref] = true
			}
		}
		shadow.Add(scan.result)
	}
	for ref := range stale {
		if !found[ref] {
			b.Remove(ref)
		}
	}
	return nil
}

func hidden(shadow *layerscan.Shadow, locations []string) bool {
	if len(locations) == 0 {
		return false
	}
	for _, location := range locations {
		if !shadow.Hides(location) {
			return false
		}
	}
	return true
}

func operatingSystemComponent(distro *layerscan.Distro) Component {
	c := Component{
		Ref:         "os:" + distro.ID + "@" + distro.VersionID,
		Type:        TypeOperatingSystem,
		Name:        distro.ID,
		Version:     distro.VersionID,
		Description: distro.PrettyName,
	}
	if distro.CPEName != "" {
		c.CPEs = []string{distro.CPEName}
	}
	return c
}

func scannedComponent(p layerscan.Package, distro *layerscan.Distro, layerDigest string) Component {
	purl := p.PURL
	if purl == "" && p.PURLTemplate != nil {
		purl = layerscan.FillPURL(*p.PURLTemplate, distro)
	}
	c := Component{
		Ref:      purl,
		Type:     p.Kind,
		Name:     p.Name,
		Version:  p.Version,
		PURL:     purl,
		CPEs:     p.CPEs,
		Supplier: p.Supplier,
	}
	if c.Type == "" {
		c.Type = TypeLibrary
	}
	if c.Ref == "" {
		// Without a package URL, a package is identified by what was found:
		// its type, name and version, and where (a binary without version
		// information, say).
		c.Ref = "scan:" + p.Type + ":" + p.Name + "@" + p.Version
		if p.Version == "" && len(p.Locations) > 0 {
			c.Ref += ":" + strings.Join(p.Locations, ",")
		}
		if layerDigest != "" && p.Version == "" && len(p.Locations) == 0 {
			c.Ref += ":" + layerDigest
		}
	}
	for _, l := range p.Licenses {
		switch {
		case l.Expression != "":
			c.Licenses = append(c.Licenses, License{Expression: l.Expression})
		case l.Name != "":
			c.Licenses = append(c.Licenses, License{Name: l.Name})
		}
	}
	return c
}

// distroOf returns the operating system an SBOM's components describe, if
// any, for the package URLs of scanned packages when no layer scan says.
func distroOf(components []Component) *layerscan.Distro {
	for _, c := range components {
		if c.Type == TypeOperatingSystem && c.Name != "" {
			return &layerscan.Distro{ID: strings.ToLower(c.Name), VersionID: c.Version}
		}
	}
	return nil
}
