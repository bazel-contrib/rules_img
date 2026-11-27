package updateimages

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// command is a buildozer command and the target it applies to, in the format
// of buildozer's -f files.
type command struct {
	cmd    string
	target string
}

func (c command) String() string { return c.cmd + "|" + c.target }

func setString(attr, value string) string { return fmt.Sprintf("set %s %q", attr, value) }

func setList(attr string, values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, fmt.Sprintf("%q", value))
	}
	return fmt.Sprintf("set %s [%s]", attr, strings.Join(quoted, ","))
}

// imageRepo is the name of the repository the images extension creates for
// the image with digest.
func imageRepo(digest string) string {
	return "img_" + strings.TrimPrefix(digest, "sha256:")
}

// addCommands works out the buildozer commands that make the changes of the
// results, and records them on the results. Results of entries that can't be
// edited get none.
func addCommands(ws *Workspace, results []*result) []command {
	var all []command
	seen := map[command]bool{}
	emit := func(r *result, c command) {
		if seen[c] {
			return
		}
		seen[c] = true
		all = append(all, c)
		r.Commands = append(r.Commands, c.String())
	}
	for _, r := range results {
		entry := r.Entry
		if r.Status != StatusChanged || !entry.Root || entry.File == "" {
			continue
		}
		target := entry.Target()
		if r.NewDigest != "" {
			emit(r, command{setString("digest", r.NewDigest), target})
		}
		if r.NewTag != "" {
			emit(r, command{setString("tag", r.NewTag), target})
		}
		if r.RemoveUnsafe {
			emit(r, command{"remove unsafe_allow_tag_without_digest", target})
		}
		if r.NewManifest != "" {
			emit(r, command{setString("manifest", r.NewManifest), target})
		}
		if r.SetReferrers != nil {
			if len(*r.SetReferrers) == 0 {
				emit(r, command{"remove referrer_digests", target})
			} else {
				emit(r, command{setList("referrer_digests", *r.SetReferrers), target})
			}
		}
		if len(r.AddReferrers) > 0 {
			emit(r, command{"add referrer_digests " + strings.Join(r.AddReferrers, " "), target})
		}
		if entry.Kind == KindImagesPull && entry.Digest != "" && r.NewDigest != "" {
			for _, c := range useRepoCommands(ws, results, r) {
				emit(r, c)
			}
		}
	}
	return all
}

// useRepoCommands renames the image repository of an images.pull tag whose
// digest changes in the use_repo calls that import it.
func useRepoCommands(ws *Workspace, results []*result, r *result) []command {
	entry := r.Entry
	oldRepo, newRepo := imageRepo(entry.Digest), imageRepo(r.NewDigest)
	// Another tag may pull the same image and keep doing so.
	for _, other := range ws.Entries {
		if other == entry || other.Kind != KindImagesPull || other.Digest != entry.Digest {
			continue
		}
		follows := false
		for _, o := range results {
			if o.Entry == other && o.Status == StatusChanged && o.NewDigest == r.NewDigest {
				follows = true
			}
		}
		if !follows {
			r.note("%s %s also pulls %s, so the use_repo imports of %s are left as they are", other.Kind, other.DisplayName(), entry.Digest, oldRepo)
			return nil
		}
	}
	var commands []command
	for _, useRepo := range ws.UseRepos {
		if useRepo.File != entry.File || useRepo.Proxy != entry.proxy {
			continue
		}
		keywords := make([]string, 0, len(useRepo.Keywords))
		for keyword := range useRepo.Keywords {
			keywords = append(keywords, keyword)
		}
		sort.Strings(keywords)
		for _, keyword := range keywords {
			if useRepo.Keywords[keyword] == oldRepo {
				commands = append(commands, command{setString(keyword, newRepo), useRepo.Target()})
			}
		}
		for _, repo := range useRepo.Positional {
			if repo == oldRepo {
				fileTarget := "//" + entry.File + ":all"
				commands = append(commands,
					command{fmt.Sprintf("use_repo_remove %s %s", entry.proxy, oldRepo), fileTarget},
					command{fmt.Sprintf("use_repo_add %s %s", entry.proxy, newRepo), fileTarget},
				)
			}
		}
	}
	return commands
}

// runBuildozer applies the commands with the buildozer binary.
func runBuildozer(buildozer, workspace string, commands []command) error {
	path, err := exec.LookPath(buildozer)
	if err != nil {
		return fmt.Errorf("buildozer is needed to apply the changes, but %q was not found (install it, pass --buildozer, or use --mode=commands to print the commands instead): %w", buildozer, err)
	}
	var script bytes.Buffer
	for _, c := range commands {
		fmt.Fprintln(&script, c)
	}
	file, err := os.CreateTemp("", "update-images-*.buildozer")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(script.Bytes()); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	cmd := exec.Command(path, "-root_dir", workspace, "-f", file.Name())
	cmd.Dir = workspace
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 3 {
		// Buildozer's exit code when there was nothing to change.
		return nil
	}
	if err != nil {
		return fmt.Errorf("running %s -f %s in %s: %w", filepath.Base(path), file.Name(), workspace, err)
	}
	return nil
}
