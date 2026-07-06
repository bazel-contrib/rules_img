// Package structuretest defines the JSON wire types shared between the
// image_structure_test rule/aspect, the `img oci-layout-metadata` producer, and
// the `img image-structure-test` consumer. Keeping them in one place ensures the
// images.json written for an OCI layout stays compatible with the reader.
package structuretest

// Request is the single file the image_structure_test rule hands to
// `img image-structure-test`. It references the aspect-produced images Spec and
// the container-structure-test config files by their runfiles rlocation paths.
type Request struct {
	// Spec is the rlocation path of the aspect-produced images spec file.
	Spec string `json:"spec"`
	// Configs are the rlocation paths of the CST config files (YAML or JSON).
	Configs []string `json:"configs"`
}

// Spec is the set of images to validate. Images known at analysis time
// (rules_img image_manifest / image_index) are listed directly in Images; for an
// OCI image layout directory (rules_oci) the platforms are discovered at build
// time by `img oci-layout-metadata`, so its metadata output tree is referenced in
// LayoutTrees and its own images.json (a Spec with only Images) is read at run
// time.
type Spec struct {
	Images      []ImageSpec `json:"images,omitempty"`
	LayoutTrees []string    `json:"layout_trees,omitempty"`
}

// ImageSpec describes a single image (one platform) to validate.
type ImageSpec struct {
	Platform Platform `json:"platform"`
	// Config and Mtree are runfiles rlocation paths at the top level of a Spec, or
	// paths relative to the metadata tree inside a LayoutTree's images.json. Mtree
	// may be empty when no mtree could be produced for the image.
	Config string `json:"config"`
	Mtree  string `json:"mtree"`
	// Complete is false when the mtree reflects only a subset of the image's
	// layers, so `shouldExist: false` assertions cannot be trusted.
	Complete bool `json:"complete"`
}

// Platform identifies the OS/arch/variant an image targets.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// String renders the platform as os/arch[/variant].
func (p Platform) String() string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}
