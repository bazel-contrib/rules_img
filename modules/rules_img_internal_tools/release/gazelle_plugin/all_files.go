package all_files

import (
	"flag"

	"github.com/bazelbuild/bazel-gazelle/config"
	"github.com/bazelbuild/bazel-gazelle/label"
	"github.com/bazelbuild/bazel-gazelle/language"
	"github.com/bazelbuild/bazel-gazelle/repo"
	"github.com/bazelbuild/bazel-gazelle/resolve"
	"github.com/bazelbuild/bazel-gazelle/rule"
)

const (
	// Directive to exclude a BUILD file from release_files
	excludeFromReleaseDirective = "exclude_from_release"

	// Language name
	languageName = "all_files"
)

// allFilesConfig stores configuration for the all_files plugin
type allFilesConfig struct {
	// Currently no global configuration needed
}

// allFilesLang implements language.Language for generating all_files targets
type allFilesLang struct{}

// NewLanguage returns a new instance of the all_files language extension
func NewLanguage() language.Language {
	return &allFilesLang{}
}

// Kinds returns the kinds of rules that this extension generates.
func (l *allFilesLang) Kinds() map[string]rule.KindInfo {
	return map[string]rule.KindInfo{
		"filegroup": {
			MatchAny: false,
			NonEmptyAttrs: map[string]bool{
				"srcs": true,
			},
			SubstituteAttrs: map[string]bool{
				"srcs": true,
			},
			ResolveAttrs: map[string]bool{
				"visibility": false,
			},
		},
	}
}

// Loads returns load statements that are required for the rules this extension generates
func (l *allFilesLang) Loads() []rule.LoadInfo {
	return nil // filegroup is built-in, no load required
}

// Name returns the name of the language
func (l *allFilesLang) Name() string {
	return languageName
}

// RegisterFlags registers command-line flags for the extension
func (l *allFilesLang) RegisterFlags(fs *flag.FlagSet, cmd string, c *config.Config) {
	// No custom flags needed
}

// CheckFlags validates the flags
func (l *allFilesLang) CheckFlags(fs *flag.FlagSet, c *config.Config) error {
	return nil
}

// KnownDirectives returns a list of directive keys that this extension uses
func (l *allFilesLang) KnownDirectives() []string {
	return []string{excludeFromReleaseDirective}
}

// Configure modifies the configuration using directives and other information
func (l *allFilesLang) Configure(c *config.Config, rel string, f *rule.File) {
	// No global configuration needed - directives are evaluated per-directory in GenerateRules
}

// GenerateRules extracts build metadata from source files in a directory
func (l *allFilesLang) GenerateRules(args language.GenerateArgs) language.GenerateResult {
	var rules []*rule.Rule
	var empty []*rule.Rule

	// Only operate on directories that already have BUILD files
	if args.File == nil {
		return language.GenerateResult{
			Gen:     rules,
			Empty:   empty,
			Imports: nil,
		}
	}

	// Check if this specific directory is excluded from release
	excludeFromRelease := false
	if args.File != nil {
		for _, d := range args.File.Directives {
			if d.Key == excludeFromReleaseDirective {
				excludeFromRelease = true
				break
			}
		}
	}

	// Check if all_files target already exists
	hasAllFiles := false
	for _, r := range args.File.Rules {
		if r.Name() == "all_files" && r.Kind() == "filegroup" {
			hasAllFiles = true
			break
		}
	}

	// Log packages that should be excluded from release_files
	if excludeFromRelease {
		pkgName := args.Rel
		if pkgName == "" {
			pkgName = "//" // Root package
		}
	}

	// Only create all_files if it doesn't exist and the package is not excluded
	if !hasAllFiles && !excludeFromRelease {
		allFilesRule := rule.NewRule("filegroup", "all_files")
		allFilesRule.SetAttr("srcs", &rule.GlobValue{
			Patterns: []string{"**"},
		})
		allFilesRule.SetAttr("visibility", []string{"//img/private/release:__subpackages__"})
		rules = append(rules, allFilesRule)
	}

	// Return empty imports array matching the number of generated rules
	// This satisfies Gazelle's validation that expects imports for each generated rule
	imports := make([]interface{}, len(rules))

	return language.GenerateResult{
		Gen:     rules,
		Empty:   empty,
		Imports: imports,
	}
}

// Fix repairs deprecated usage of language-specific rules
func (l *allFilesLang) Fix(c *config.Config, f *rule.File) {
	// No deprecated usage to fix
}

// Imports returns a list of imports in the given rule
func (l *allFilesLang) Imports(c *config.Config, r *rule.Rule, f *rule.File) []resolve.ImportSpec {
	return nil
}

// Embeds returns a list of labels of rules that the given rule embeds
func (l *allFilesLang) Embeds(r *rule.Rule, from label.Label) []label.Label {
	return nil
}

// Resolve translates import paths into Bazel labels
func (l *allFilesLang) Resolve(c *config.Config, ix *resolve.RuleIndex, rc *repo.RemoteCache, r *rule.Rule, imports interface{}, from label.Label) {
	// No imports to resolve
}
