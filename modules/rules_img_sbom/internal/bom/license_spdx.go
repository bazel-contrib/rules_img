// Copied from https://github.com/bazel-contrib/supply-chain/blob/c18a7daa7a4ed5e6d523a19f070c0cba19118419/lib/supplychain-go/cmd/spdx/license_2_3.go
// (Apache-2.0): its package is a main package and cannot be imported.

package bom

import (
	"fmt"
	"os"
	"strings"

	supplychain "github.com/bazel-contrib/supply-chain/lib/supplychain-go"
	spdx "github.com/spdx/tools-golang/spdx"
)

// declaredLicenses23 sets the declared license of the packages, as an SPDX
// license expression, and returns the licenses and exceptions that are not on
// the SPDX list, which SPDX 2.3 defines in the document as extracted licensing
// information.
//
// A custom id can be declared with different texts (e.g. different copyright
// lines); each text gets its own entry, and the ids after the first are
// suffixed to keep them unique.
func declaredLicenses23(packages []*spdx.Package, licenseOf map[*spdx.Package]*supplychain.LicenseAttribute) ([]*spdx.OtherLicense, error) {
	c := &licenseCollector23{
		idOf:     make(map[licenseKey]string),
		variants: make(map[string]int),
	}
	for _, pkg := range packages {
		attribute, ok := licenseOf[pkg]
		if !ok {
			continue
		}
		text := "NOASSERTION"
		if attribute.Text != "" {
			content, err := os.ReadFile(attribute.Text)
			if err != nil {
				return nil, fmt.Errorf("reading the license text of %s: %w", pkg.PackageName, err)
			}
			text = string(content)
		}
		declared, err := c.render(attribute.Root(), text)
		if err != nil {
			return nil, fmt.Errorf("license of %s: %w", pkg.PackageName, err)
		}
		pkg.PackageLicenseDeclared = declared
	}
	return c.others, nil
}

type licenseCollector23 struct {
	others   []*spdx.OtherLicense
	idOf     map[licenseKey]string
	variants map[string]int
}

// render writes the expression, with the ids of its custom licenses. It fails
// on an expression that SPDX 2.3 cannot represent.
func (c *licenseCollector23) render(e supplychain.LicenseExpression, text string) (string, error) {
	switch {
	case e.IsSingle():
		return c.leaf(e, text), nil
	case e.Operator == supplychain.LicenseWith:
		if e.Exception.IsCustom() {
			return "", fmt.Errorf("the custom exception %s in %q cannot be represented in SPDX 2.3, which only allows exceptions of the SPDX list; use format = \"json-ld\" (SPDX 3.0.1) or an exception of the SPDX list", e.Exception.Identifier, e.String())
		}
		return c.leaf(e.Operands[0], text) + " WITH " + e.Exception.Identifier, nil
	}
	parts := make([]string, len(e.Operands))
	for i, operand := range e.Operands {
		part, err := c.render(operand, text)
		if err != nil {
			return "", err
		}
		parts[i] = part
		if (operand.Operator == supplychain.LicenseAnd || operand.Operator == supplychain.LicenseOr) && operand.Operator != e.Operator {
			parts[i] = "(" + parts[i] + ")"
		}
	}
	return strings.Join(parts, " "+string(e.Operator)+" "), nil
}

func (c *licenseCollector23) leaf(e supplychain.LicenseExpression, text string) string {
	if !e.Kind.IsCustom() {
		return e.LeafID()
	}
	if e.OrLater {
		// SPDX only allows `+` after a license of the SPDX list.
		return c.define(*e.Kind, e.Kind.RefID(false)+"-or-later", text)
	}
	return c.define(*e.Kind, e.Kind.RefID(false), text)
}

// define returns the id of the entry of a custom license, creating it if it is
// new. Listed ones are returned as they are.
func (c *licenseCollector23) define(kind supplychain.LicenseKind, refID, text string) string {
	if !kind.IsCustom() {
		return kind.Identifier
	}
	key := licenseKey{id: refID, text: text}
	if id, ok := c.idOf[key]; ok {
		return id
	}
	id := refID
	if n := c.variants[refID]; n > 0 {
		id = fmt.Sprintf("%s-%d", refID, n)
	}
	c.variants[refID]++
	c.idOf[key] = id
	c.others = append(c.others, &spdx.OtherLicense{LicenseIdentifier: id, ExtractedText: text, LicenseName: nameOf(kind)})
	return id
}

// Also copied from license_3_0_1.go of the same commit.
type licenseKey struct {
	id, text  string
	exception bool
}

func nameOf(kind supplychain.LicenseKind) string {
	if kind.Name == "" {
		return kind.Identifier
	}
	return kind.Name
}
