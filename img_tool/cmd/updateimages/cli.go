package updateimages

import (
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// commandSpec describes a command of img update-images: what its help says,
// and which flags it takes.
type commandSpec struct {
	name string
	// summary is the line of the command in the overview.
	summary string
	// description opens the help of the command.
	description string
	// changes tells whether the command proposes changes (and so takes
	// --mode and --buildozer).
	changes bool
	// flags are the names of the flags of the command, in the order of its
	// help. The flags of every command are added.
	flags []string
}

// pulledImages says which images the commands work on.
const pulledImages = `The pulled images are those that the root module declares in MODULE.bazel (and
the files it includes) with the pull repository rule and the images.pull and
images.referrers tags of the images extension.`

var commandSpecs = []commandSpec{
	{
		name:    "update",
		summary: "moves images to newer versions",
		description: `Updates the pulled images to newer versions:

  - An image with a tag that contains a version moves to the highest tag of the
    same shape: 3.23 -> 3.24, v1.2.3 -> v1.4.0, 1.27-alpine -> 1.28-alpine
    (but not 3.23 -> 3.24.1 or 3.24-rc1). With --keep-tags, or if there is no
    newer version, it keeps its tag and gets the digest the tag points to now.
  - An image with any other tag, like "latest", gets the digest its tag points
    to now.
  - An image with only a digest gets the digest that "latest" points to
    (see --latest-tag).
  - An image with only a tag moves to a newer version tag, but doesn't get a
    digest: that is what pin does.

When the digest of an image changes, everything that names the old digest
follows: each pinned referrer in referrer_digests is replaced by the referrers
of the same type of the corresponding manifest of the new image,
images.referrers tags move to the corresponding new manifest, and use_repo
imports of the image's repository are renamed.

The images that other modules pull through the images extension are checked
too, but can't be changed.`,
		changes: true,
		flags:   []string{"keep-tags", "max-bump", "latest-tag", "root-only", "jobs"},
	},
	{
		name:    "pin",
		summary: "adds a digest to images that only have a tag",
		description: `Pins the pulled images that only have a tag: each gets the digest its tag
points to now, and loses unsafe_allow_tag_without_digest, which doesn't apply
to an image with a digest.

Only the files of the root module are read, so this works even while the images
extension fails to evaluate, for example because an images.pull has neither a
digest nor unsafe_allow_tag_without_digest.`,
		changes: true,
		flags:   []string{"jobs"},
	},
	{
		name:    "referrers",
		summary: "pins the referrers of images (signatures, SBOMs, attestations)",
		description: `Finds the referrers of the pulled images, and adds those not pinned yet to
referrer_digests.

The referrers of an image, of its child manifests if it is an index, and of the
manifest of every images.referrers tag are found with:

  - the registry's referrers API, or the referrers tag schema (sha256-<hex>)
    if the registry has no such API, as the OCI distribution spec says,
  - cosign's tag scheme (sha256-<hex>.sig and sha256-<hex>.att), whose
    artifacts must name the manifest in their signed payloads.

For each referrer, the report shows its artifact type, how it was found, its
size, annotations and layers, and whether it is pinned already. --artifact-type
selects referrers by type.

The referrers of images that other modules pull through the images extension
are shown too, with an images.referrers tag that pins them from the root
module.`,
		changes: true,
		flags:   []string{"artifact-type", "root-only", "jobs"},
	},
	{
		name:    "list",
		summary: "lists the pulled images, without accessing any registry",
		description: `Lists the pulled images, and the images that other modules pull through the
images extension. No registry is accessed.`,
		flags: []string{"root-only"},
	},
}

func commandSpecNamed(name string) (commandSpec, bool) {
	for _, spec := range commandSpecs {
		if spec.name == name {
			return spec, true
		}
	}
	return commandSpec{}, false
}

type options struct {
	command string
	// bazelTarget is the label of the target that runs the command, for help
	// and hints.
	bazelTarget   string
	workspace     string
	inventory     string
	mode          string
	format        string
	buildozer     string
	jobs          int
	rootOnly      bool
	artifactTypes stringList
	maxBump       string
	keepTags      bool
	latestTag     string
	patterns      []string
	// userArgs are the flags and patterns given on the command line, without
	// those of the Bazel target and --mode.
	userArgs []string
}

// applyCommand is the command line that makes the changes the report shows:
// the same command, flags and patterns, with --mode=apply.
func (o *options) applyCommand() string {
	words := []string{invocation(o.command, o.bazelTarget)}
	if o.bazelTarget != "" {
		words = append(words, "--")
	}
	words = append(words, "--mode=apply")
	for _, arg := range o.userArgs {
		words = append(words, shellQuote(arg))
	}
	return strings.Join(words, " ")
}

// shellQuote quotes s for a POSIX shell, if it needs quoting.
func shellQuote(s string) string {
	safe := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./_-", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// recordUserArgs records the flags that were set (other than those of the
// Bazel target and --mode) and the patterns, to repeat them in applyCommand.
func (o *options) recordUserArgs(flags *flag.FlagSet) {
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "mode" || slices.Contains(hiddenFlags, f.Name) {
			return
		}
		if list, ok := f.Value.(*stringList); ok {
			for _, value := range *list {
				o.userArgs = append(o.userArgs, "--"+f.Name+"="+value)
			}
			return
		}
		if getter, ok := f.Value.(flag.Getter); ok {
			if b, ok := getter.Get().(bool); ok && b {
				o.userArgs = append(o.userArgs, "--"+f.Name)
				return
			}
		}
		o.userArgs = append(o.userArgs, "--"+f.Name+"="+f.Value.String())
	})
	o.userArgs = append(o.userArgs, o.patterns...)
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// hiddenFlags are set by the Bazel targets, and not shown in their help.
var hiddenFlags = []string{"bazel-target", "inventory"}

var flagDefinitions = map[string]func(*flag.FlagSet, *options){
	"bazel-target": func(f *flag.FlagSet, o *options) {
		f.StringVar(&o.bazelTarget, "bazel-target", "", "The label of the Bazel target running the command")
	},
	"inventory": func(f *flag.FlagSet, o *options) {
		f.StringVar(&o.inventory, "inventory", "", "inventory.json of the images extension, listing the images of every module")
	},
	"mode": func(f *flag.FlagSet, o *options) {
		f.StringVar(&o.mode, "mode", "report", "What to do with the changes (report, commands, apply): report them, print the buildozer commands that make them, or apply them with buildozer")
	},
	"format": func(f *flag.FlagSet, o *options) {
		f.StringVar(&o.format, "format", "text", "Format of the report: text or json")
	},
	"buildozer": func(f *flag.FlagSet, o *options) {
		f.StringVar(&o.buildozer, "buildozer", "buildozer", "The buildozer binary that --mode=apply runs")
	},
	"workspace": func(f *flag.FlagSet, o *options) {
		f.StringVar(&o.workspace, "workspace", "", "The workspace whose MODULE.bazel to read (default: the workspace bazel run runs in)")
	},
	"jobs": func(f *flag.FlagSet, o *options) {
		f.IntVar(&o.jobs, "jobs", 8, "How many images to work on at once")
	},
	"root-only": func(f *flag.FlagSet, o *options) {
		f.BoolVar(&o.rootOnly, "root-only", false, "Leave out the images that other modules pull")
	},
	"artifact-type": func(f *flag.FlagSet, o *options) {
		f.Var(&o.artifactTypes, "artifact-type", "Only consider referrers of this artifact `type` (repeatable). * matches anything; cosign:sig and cosign:att are cosign's signatures and attestations, buildkit:attestation the BuildKit attestations an index lists")
	},
	"keep-tags": func(f *flag.FlagSet, o *options) {
		f.BoolVar(&o.keepTags, "keep-tags", false, "Never move a tag: only update digests to what the current tags point to (same as --max-bump=none)")
	},
	"max-bump": func(f *flag.FlagSet, o *options) {
		f.StringVar(&o.maxBump, "max-bump", "major", "How much of a version tag may change (a `level`): major, minor (keeps the first number), patch (keeps the first two), or none")
	},
	"latest-tag": func(f *flag.FlagSet, o *options) {
		f.StringVar(&o.latestTag, "latest-tag", "latest", "The tag to follow for images with a digest but no tag")
	},
}

// visibleFlags are the flags in the help of the command.
func (spec commandSpec) visibleFlags() []string {
	flags := slices.Clone(spec.flags)
	if spec.changes {
		flags = append([]string{"mode"}, flags...)
		flags = append(flags, "buildozer")
	}
	return append(flags, "format", "workspace")
}

func newFlagSet(spec commandSpec, opts *options) *flag.FlagSet {
	flags := flag.NewFlagSet(spec.name, flag.ContinueOnError)
	// Errors are reported by parseArgs, in one line rather than with the whole
	// help.
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	for _, name := range append(spec.visibleFlags(), hiddenFlags...) {
		flagDefinitions[name](flags, opts)
	}
	return flags
}

// invocation is how the command is run: with bazel run, if the command knows
// its target, or else directly.
func invocation(command, bazelTarget string) string {
	if bazelTarget == "" {
		return "img update-images " + command
	}
	return "bazel run " + siblingTarget(bazelTarget, command)
}

// helpInvocation is how to show the help of command.
func helpInvocation(command, bazelTarget string) string {
	if bazelTarget == "" {
		return invocation(command, "") + " --help"
	}
	return invocation(command, bazelTarget) + " -- --help"
}

// siblingTarget is the target in the same package as bazelTarget that runs
// command: the targets are named after their commands.
func siblingTarget(bazelTarget, command string) string {
	if i := strings.LastIndex(bazelTarget, ":"); i >= 0 {
		return bazelTarget[:i+1] + command
	}
	return bazelTarget + ":" + command
}

func writeCommandHelp(w io.Writer, spec commandSpec, bazelTarget string, flags *flag.FlagSet) {
	run := invocation(spec.name, bazelTarget)
	if bazelTarget != "" {
		run += " --"
	}
	fmt.Fprintf(w, "Usage: %s [FLAGS] [PATTERN...]\n\n%s\n\n%s\n\n", run, spec.description, pulledImages)
	fmt.Fprintf(w, "PATTERNs select images by name, repository or digest, with * as a wildcard.\nWithout any, every image is selected.\n\n")
	if spec.changes {
		fmt.Fprintf(w, "Changes are only reported, unless --mode=apply makes them with buildozer,\nwhich must be in PATH.\n\n")
	}
	fmt.Fprintf(w, "Flags:\n")
	for _, name := range spec.visibleFlags() {
		f := flags.Lookup(name)
		typ, usage := flag.UnquoteUsage(f)
		fmt.Fprintf(w, "  --%s", f.Name)
		if typ != "" {
			fmt.Fprintf(w, " %s", typ)
		}
		fmt.Fprintf(w, "\n    \t%s", usage)
		isString := false
		if getter, ok := f.Value.(flag.Getter); ok {
			_, isString = getter.Get().(string)
		}
		switch {
		case f.DefValue == "" || f.DefValue == "false" || f.DefValue == "0" || f.DefValue == "[]":
		case isString:
			fmt.Fprintf(w, " (default %q)", f.DefValue)
		default:
			fmt.Fprintf(w, " (default %s)", f.DefValue)
		}
		fmt.Fprintln(w)
	}
	if bazelTarget != "" {
		var others []string
		for _, other := range commandSpecs {
			if other.name != spec.name {
				others = append(others, fmt.Sprintf("  %-38s %s", siblingTarget(bazelTarget, other.name), other.summary))
			}
		}
		fmt.Fprintf(w, "\nSee also:\n%s\n", strings.Join(others, "\n"))
	}
}

func writeOverview(w io.Writer) {
	fmt.Fprintf(w, "Usage: img update-images COMMAND [FLAGS] [PATTERN...]\n\n")
	fmt.Fprintf(w, "Maintains the pulled images. %s\n\nCommands:\n", pulledImages)
	for _, spec := range commandSpecs {
		fmt.Fprintf(w, "  %-10s %s\n", spec.name, spec.summary)
	}
	fmt.Fprintf(w, "\nWith Bazel, run them as bazel run @rules_img//img/pull:COMMAND.\nRun a command with --help for its flags.\n")
}

func isHelp(arg string) bool {
	return arg == "help" || arg == "-h" || arg == "-help" || arg == "--help"
}

// parseArgs parses the arguments of img update-images. Help that is asked for
// goes to stdout; help after a mistake to stderr.
func parseArgs(args []string, stdout, stderr io.Writer) (*options, error) {
	if len(args) == 0 {
		writeOverview(stderr)
		return nil, fmt.Errorf("missing command")
	}
	if isHelp(args[0]) {
		writeOverview(stdout)
		return nil, flag.ErrHelp
	}
	spec, ok := commandSpecNamed(args[0])
	if !ok {
		writeOverview(stderr)
		return nil, fmt.Errorf("unknown command %q", args[0])
	}
	opts := &options{command: spec.name, mode: "report", format: "text", jobs: 8, maxBump: "major", latestTag: "latest"}
	flags := newFlagSet(spec, opts)
	rest := args[1:]
	for {
		err := flags.Parse(rest)
		if err == flag.ErrHelp {
			writeCommandHelp(stdout, spec, opts.bazelTarget, flags)
			return nil, err
		}
		if err != nil {
			return nil, fmt.Errorf("%v (see: %s)", err, helpInvocation(spec.name, opts.bazelTarget))
		}
		rest = flags.Args()
		if len(rest) == 0 {
			break
		}
		opts.patterns = append(opts.patterns, rest[0])
		rest = rest[1:]
	}
	opts.recordUserArgs(flags)
	if !slices.Contains([]string{"report", "commands", "apply"}, opts.mode) {
		return nil, fmt.Errorf("invalid --mode %q: want report, commands or apply", opts.mode)
	}
	if !slices.Contains([]string{"text", "json"}, opts.format) {
		return nil, fmt.Errorf("invalid --format %q: want text or json", opts.format)
	}
	if opts.workspace == "" {
		opts.workspace = os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	}
	if opts.workspace == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		opts.workspace = wd
	}
	return opts, nil
}
