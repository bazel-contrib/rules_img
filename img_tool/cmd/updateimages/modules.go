package updateimages

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/modulefile"
)

// Kind is the kind of call that declares an image.
type Kind string

const (
	// KindPull is a call of the pull repository rule, via use_repo_rule.
	KindPull Kind = "pull"
	// KindImagesPull is an images.pull tag of the images module extension.
	KindImagesPull Kind = "images.pull"
	// KindImagesReferrers is an images.referrers tag of the images module
	// extension, which attaches referrers to a manifest pulled by any module.
	KindImagesReferrers Kind = "images.referrers"
)

const defaultRegistry = "index.docker.io"

// Entry is one image declaration (or images.referrers tag).
type Entry struct {
	Kind Kind `json:"kind"`
	// Module and Version identify the module that declares the entry. They are
	// empty for entries of the root module that were found by parsing its files.
	Module  string `json:"module,omitempty"`
	Version string `json:"version,omitempty"`
	// Root tells whether the root module declares the entry. Only those can be
	// edited.
	Root bool `json:"root"`
	// File (relative to the workspace root) and Line locate the call.
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`

	Name                        string   `json:"name,omitempty"`
	Registry                    string   `json:"registry,omitempty"`
	Registries                  []string `json:"registries,omitempty"`
	Repository                  string   `json:"repository,omitempty"`
	Tag                         string   `json:"tag,omitempty"`
	Digest                      string   `json:"digest,omitempty"`
	Manifest                    string   `json:"manifest,omitempty"`
	ReferrerDigests             []string `json:"referrer_digests,omitempty"`
	ReferrerArtifactTypes       []string `json:"referrer_artifact_types,omitempty"`
	ReferrerCosignTags          []string `json:"referrer_cosign_tags,omitempty"`
	ReferrerBuildKit            bool     `json:"referrer_buildkit_attestations,omitempty"`
	HasReferrerArtifacts        bool     `json:"has_referrer_artifacts,omitempty"`
	Platforms                   []string `json:"platforms,omitempty"`
	LayerHandling               string   `json:"layer_handling,omitempty"`
	UnsafeAllowTagWithoutDigest bool     `json:"unsafe_allow_tag_without_digest,omitempty"`
	DevDependency               bool     `json:"dev_dependency,omitempty"`

	// proxy is the name of the extension proxy the tag was called on.
	proxy string
	// present and literal record which attributes are set, and which of them
	// are written out as literals (so they can be rewritten in place).
	present map[string]bool
	literal map[string]bool
	// unresolved are the attributes whose value could not be evaluated.
	unresolved []string
}

// DisplayName is the name the entry is referred to by: its name, its
// repository, or (for images.referrers) its manifest.
func (e *Entry) DisplayName() string {
	switch {
	case e.Name != "":
		return e.Name
	case e.Repository != "":
		return e.Repository
	}
	return e.Manifest
}

// Where says where the entry is declared.
func (e *Entry) Where() string {
	if e.File != "" {
		return fmt.Sprintf("%s:%d", e.File, e.Line)
	}
	if e.Version != "" {
		return e.Module + "@" + e.Version
	}
	return e.Module
}

// Target is the buildozer target of the call.
func (e *Entry) Target() string {
	return fmt.Sprintf("//%s:%%%d", e.File, e.Line)
}

// Editable tells whether attribute attr can be set in place: the entry is in
// the root module and the attribute is either unset or a literal.
func (e *Entry) Editable(attr string) bool {
	return e.Root && e.File != "" && (!e.present[attr] || e.literal[attr])
}

// Sources are the places to pull the image from, in the order they are tried.
func (e *Entry) Sources() []source {
	var registries []string
	if e.Registry != "" {
		registries = append(registries, e.Registry)
	}
	registries = append(registries, e.Registries...)
	if len(registries) == 0 {
		registries = []string{defaultRegistry}
	}
	sources := make([]source, 0, len(registries))
	for _, registry := range registries {
		sources = append(sources, source{Registry: registry, Repository: e.Repository})
	}
	return sources
}

// Ref is a human readable reference: the first source, tag and digest.
func (e *Entry) Ref() string {
	if e.Kind == KindImagesReferrers {
		return "@" + e.Manifest
	}
	ref := e.Sources()[0].String()
	if e.Tag != "" {
		ref += ":" + e.Tag
	}
	if e.Digest != "" {
		ref += "@" + e.Digest
	}
	return ref
}

// source is a registry and a repository on it.
type source struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
}

func (s source) String() string { return s.Registry + "/" + s.Repository }

// UseRepo is a use_repo call.
type UseRepo struct {
	File       string
	Line       int
	Proxy      string
	Positional []string
	// Keywords maps the apparent names of imported repositories to their
	// names in the extension.
	Keywords map[string]string
}

// Target is the buildozer target of the use_repo call.
func (u *UseRepo) Target() string {
	return fmt.Sprintf("//%s:%%%d", u.File, u.Line)
}

// Settings are the attributes of the root module's images.settings tag that
// affect registry access.
type Settings struct {
	CredentialHelper string
	DockerConfigPath string
	// Config is the path of the file that the config attribute labels,
	// relative to the workspace root.
	Config string
}

// Workspace is what was found in the MODULE.bazel files of the root module.
type Workspace struct {
	Root     string
	Files    []string
	Entries  []*Entry
	UseRepos []*UseRepo
	Settings Settings
}

type parsedFile struct {
	rel  string
	file *modulefile.File
}

type scanner struct {
	root  string
	files []parsedFile
	seen  map[string]bool
	// moduleName is the name of the root module; rulesImgRepos are the
	// apparent names under which it sees rules_img.
	moduleName     string
	moduleRepoName string
	rulesImgRepos  map[string]bool
}

// ScanWorkspace parses the MODULE.bazel file of the workspace at root, and
// every file it includes, and returns the images they declare.
func ScanWorkspace(root string) (*Workspace, error) {
	s := &scanner{root: root, seen: map[string]bool{}, rulesImgRepos: map[string]bool{"rules_img": true}}
	if err := s.parse("MODULE.bazel"); err != nil {
		return nil, err
	}
	// Repository names are module-wide, so collect them from every file before
	// resolving any label.
	for _, f := range s.files {
		for _, stmt := range f.file.Stmts {
			call := stmtCall(stmt)
			if call == nil {
				continue
			}
			switch calleeName(call) {
			case "module":
				s.moduleName, _ = modulefile.AsString(modulefile.Eval(call.Keyword("name"), nil))
				s.moduleRepoName, _ = modulefile.AsString(modulefile.Eval(call.Keyword("repo_name"), nil))
			case "bazel_dep":
				name, _ := modulefile.AsString(modulefile.Eval(call.Keyword("name"), nil))
				if name != "rules_img" {
					continue
				}
				if repoName, ok := modulefile.AsString(modulefile.Eval(call.Keyword("repo_name"), nil)); ok && repoName != "" {
					delete(s.rulesImgRepos, "rules_img")
					s.rulesImgRepos[repoName] = true
				}
			}
		}
	}
	ws := &Workspace{Root: root}
	for _, f := range s.files {
		ws.Files = append(ws.Files, f.rel)
		s.collect(f, ws)
	}
	return ws, nil
}

func (s *scanner) parse(rel string) error {
	if s.seen[rel] {
		return nil
	}
	s.seen[rel] = true
	data, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("reading %s: %w", rel, err)
	}
	file, err := modulefile.Parse(rel, data)
	if err != nil {
		return err
	}
	s.files = append(s.files, parsedFile{rel: rel, file: file})
	env := modulefile.Env{}
	for _, stmt := range file.Stmts {
		if assign, ok := stmt.(*modulefile.Assign); ok {
			assign.Bind(env)
			continue
		}
		call := stmtCall(stmt)
		if call == nil || calleeName(call) != "include" {
			continue
		}
		positional := call.Positional()
		if len(positional) != 1 {
			return fmt.Errorf("%s:%d: include() takes exactly one label", rel, call.Start().Line)
		}
		label, ok := modulefile.AsString(modulefile.Eval(positional[0], env))
		if !ok {
			return fmt.Errorf("%s:%d: can't evaluate the label passed to include()", rel, call.Start().Line)
		}
		included, err := s.includePath(rel, label)
		if err != nil {
			return fmt.Errorf("%s:%d: %w", rel, call.Start().Line, err)
		}
		if err := s.parse(included); err != nil {
			return err
		}
	}
	return nil
}

// includePath resolves the label of an included file to a path relative to
// the workspace root. Included files are always in the main repository.
func (s *scanner) includePath(from, label string) (string, error) {
	pkg, name, ok := strings.Cut(label, ":")
	if !ok {
		return "", fmt.Errorf("include(%q): label has no target name", label)
	}
	switch {
	case pkg == "":
		// A label relative to the package of the including file.
		pkg = filepath.ToSlash(filepath.Dir(from))
		if pkg == "." {
			pkg = ""
		}
	case strings.HasPrefix(pkg, "//"):
		pkg = strings.TrimPrefix(pkg, "//")
	case strings.HasPrefix(pkg, "@"):
		repo, rest, ok := strings.Cut(strings.TrimLeft(pkg, "@"), "//")
		if !ok || (repo != "" && repo != s.moduleName && repo != s.moduleRepoName) {
			return "", fmt.Errorf("include(%q): can only include files of the main repository", label)
		}
		pkg = rest
	default:
		return "", fmt.Errorf("include(%q): invalid label", label)
	}
	if pkg == "" {
		return name, nil
	}
	return pkg + "/" + name, nil
}

// isRulesImgLabel tells whether label names the file at path (like
// "//img:extensions.bzl") of rules_img.
func (s *scanner) isRulesImgLabel(label, path string) bool {
	repo := ""
	rest := label
	canonical := strings.HasPrefix(label, "@@")
	if strings.HasPrefix(label, "@") {
		var ok bool
		repo, rest, ok = strings.Cut(strings.TrimLeft(label, "@"), "//")
		if !ok {
			return false
		}
		rest = "//" + rest
	}
	if rest != path {
		return false
	}
	if repo == "" {
		// The main repository.
		return s.moduleName == "rules_img"
	}
	if canonical {
		return repo == "rules_img+" || repo == "rules_img~" || repo == "rules_img"
	}
	if s.moduleName == "rules_img" && (repo == s.moduleName || repo == s.moduleRepoName) {
		return true
	}
	return s.rulesImgRepos[repo]
}

type proxyKind int

const (
	proxyImages proxyKind = iota + 1
	proxyPull
)

type proxy struct {
	kind proxyKind
	dev  bool
}

func (s *scanner) collect(f parsedFile, ws *Workspace) {
	env := modulefile.Env{}
	proxies := map[string]proxy{}
	for _, stmt := range f.file.Stmts {
		if assign, ok := stmt.(*modulefile.Assign); ok {
			if call, ok := assign.Value.(*modulefile.Call); ok && len(assign.Targets) == 1 {
				if p, ok := s.proxyOf(call, env); ok {
					proxies[assign.Targets[0]] = p
					env[assign.Targets[0]] = modulefile.Unknown{}
					continue
				}
			}
			delete(proxies, assign.Targets[0])
			assign.Bind(env)
			continue
		}
		call := stmtCall(stmt)
		if call == nil {
			continue
		}
		line := call.Start().Line
		switch fn := call.Fn.(type) {
		case *modulefile.Ident:
			if fn.Name == "use_repo" {
				if useRepo := useRepoOf(f.rel, call, env); useRepo != nil {
					ws.UseRepos = append(ws.UseRepos, useRepo)
				}
				continue
			}
			p, ok := proxies[fn.Name]
			if !ok || p.kind != proxyPull {
				continue
			}
			entry := newEntry(KindPull, f.rel, line, call, env)
			if dev, ok := modulefile.AsBool(modulefile.Eval(call.Keyword("dev_dependency"), env)); ok {
				entry.DevDependency = dev
			}
			ws.Entries = append(ws.Entries, entry)
		case *modulefile.Dot:
			ident, ok := fn.X.(*modulefile.Ident)
			if !ok {
				continue
			}
			p, ok := proxies[ident.Name]
			if !ok || p.kind != proxyImages {
				continue
			}
			switch fn.Name {
			case "pull":
				entry := newEntry(KindImagesPull, f.rel, line, call, env)
				entry.proxy = ident.Name
				entry.DevDependency = p.dev
				ws.Entries = append(ws.Entries, entry)
			case "referrers":
				entry := newEntry(KindImagesReferrers, f.rel, line, call, env)
				entry.proxy = ident.Name
				entry.DevDependency = p.dev
				ws.Entries = append(ws.Entries, entry)
			case "settings":
				ws.Settings.CredentialHelper, _ = modulefile.AsString(modulefile.Eval(call.Keyword("credential_helper"), env))
				ws.Settings.DockerConfigPath, _ = modulefile.AsString(modulefile.Eval(call.Keyword("docker_config_path"), env))
				if label, ok := modulefile.AsString(modulefile.Eval(call.Keyword("config"), env)); ok && label != "" {
					if path, err := s.includePath(f.rel, label); err == nil {
						ws.Settings.Config = path
					}
				}
			}
		}
	}
}

// proxyOf tells whether call creates a proxy for the images extension or the
// pull repository rule of rules_img.
func (s *scanner) proxyOf(call *modulefile.Call, env modulefile.Env) (proxy, bool) {
	name := calleeName(call)
	if name != "use_extension" && name != "use_repo_rule" {
		return proxy{}, false
	}
	positional := call.Positional()
	if len(positional) != 2 {
		return proxy{}, false
	}
	label, ok1 := modulefile.AsString(modulefile.Eval(positional[0], env))
	symbol, ok2 := modulefile.AsString(modulefile.Eval(positional[1], env))
	if !ok1 || !ok2 {
		return proxy{}, false
	}
	dev, _ := modulefile.AsBool(modulefile.Eval(call.Keyword("dev_dependency"), env))
	switch {
	case name == "use_extension" && symbol == "images" && s.isRulesImgLabel(label, "//img:extensions.bzl"):
		return proxy{kind: proxyImages, dev: dev}, true
	case name == "use_repo_rule" && symbol == "pull" && s.isRulesImgLabel(label, "//img:pull.bzl"):
		return proxy{kind: proxyPull, dev: dev}, true
	}
	return proxy{}, false
}

func newEntry(kind Kind, file string, line int, call *modulefile.Call, env modulefile.Env) *Entry {
	entry := &Entry{
		Kind:    kind,
		Root:    true,
		File:    file,
		Line:    line,
		present: map[string]bool{},
		literal: map[string]bool{},
	}
	str := func(attr string, dst *string) {
		x := call.Keyword(attr)
		if x == nil {
			return
		}
		entry.present[attr] = true
		entry.literal[attr] = modulefile.IsLiteral(x)
		value, ok := modulefile.AsString(modulefile.Eval(x, env))
		if !ok {
			entry.unresolved = append(entry.unresolved, attr)
			return
		}
		*dst = value
	}
	strs := func(attr string, dst *[]string) {
		x := call.Keyword(attr)
		if x == nil {
			return
		}
		entry.present[attr] = true
		entry.literal[attr] = modulefile.IsLiteral(x)
		value, ok := modulefile.AsStrings(modulefile.Eval(x, env))
		if !ok {
			entry.unresolved = append(entry.unresolved, attr)
			return
		}
		*dst = value
	}
	str("name", &entry.Name)
	str("registry", &entry.Registry)
	strs("registries", &entry.Registries)
	str("repository", &entry.Repository)
	str("tag", &entry.Tag)
	str("digest", &entry.Digest)
	str("manifest", &entry.Manifest)
	strs("referrer_digests", &entry.ReferrerDigests)
	strs("referrer_artifact_types", &entry.ReferrerArtifactTypes)
	strs("referrer_cosign_tags", &entry.ReferrerCosignTags)
	strs("platforms", &entry.Platforms)
	str("layer_handling", &entry.LayerHandling)
	if x := call.Keyword("referrer_artifacts"); x != nil {
		entry.present["referrer_artifacts"] = true
		entry.HasReferrerArtifacts = true
	}
	if x := call.Keyword("referrer_buildkit_attestations"); x != nil {
		entry.present["referrer_buildkit_attestations"] = true
		entry.literal["referrer_buildkit_attestations"] = modulefile.IsLiteral(x)
		value, ok := modulefile.AsBool(modulefile.Eval(x, env))
		if !ok {
			entry.unresolved = append(entry.unresolved, "referrer_buildkit_attestations")
		}
		entry.ReferrerBuildKit = value
	}
	if x := call.Keyword("unsafe_allow_tag_without_digest"); x != nil {
		entry.present["unsafe_allow_tag_without_digest"] = true
		entry.literal["unsafe_allow_tag_without_digest"] = modulefile.IsLiteral(x)
		value, ok := modulefile.AsBool(modulefile.Eval(x, env))
		if !ok {
			entry.unresolved = append(entry.unresolved, "unsafe_allow_tag_without_digest")
		}
		entry.UnsafeAllowTagWithoutDigest = value
	}
	sort.Strings(entry.unresolved)
	return entry
}

func useRepoOf(file string, call *modulefile.Call, env modulefile.Env) *UseRepo {
	positional := call.Positional()
	if len(positional) == 0 {
		return nil
	}
	ident, ok := positional[0].(*modulefile.Ident)
	if !ok {
		return nil
	}
	useRepo := &UseRepo{File: file, Line: call.Start().Line, Proxy: ident.Name, Keywords: map[string]string{}}
	for _, x := range positional[1:] {
		if repo, ok := modulefile.AsString(modulefile.Eval(x, env)); ok {
			useRepo.Positional = append(useRepo.Positional, repo)
		}
	}
	for _, arg := range call.Args {
		if arg.Name == "" || arg.Star != 0 {
			continue
		}
		if repo, ok := modulefile.AsString(modulefile.Eval(arg.Value, env)); ok {
			useRepo.Keywords[arg.Name] = repo
		}
	}
	return useRepo
}

func stmtCall(stmt modulefile.Stmt) *modulefile.Call {
	if exprStmt, ok := stmt.(*modulefile.ExprStmt); ok {
		call, _ := exprStmt.X.(*modulefile.Call)
		return call
	}
	return nil
}

func calleeName(call *modulefile.Call) string {
	if ident, ok := call.Fn.(*modulefile.Ident); ok {
		return ident.Name
	}
	return ""
}
