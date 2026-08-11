package main

import (
	"fmt"
	"html/template"
	"regexp"
	"strings"
)

// A version published by this tool ends in the date and short commit of the build
// it came from, which is enough to link it back to the commit.
var buildStampPattern = regexp.MustCompile(`-(\d{4})(\d{2})(\d{2})-([0-9a-f]{7,40})$`)

type indexVersion struct {
	Version   string
	Date      string
	CommitURL string
	Commit    string
}

type indexPage struct {
	Module      string
	RegistryURL string
	Repository  string
	Artifact    string
	Latest      string
	Versions    []indexVersion
}

var indexTemplate = template.Must(template.New(indexHTML).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Module}} pre-release registry</title>
<style>
:root { color-scheme: light dark; }
body { font-family: system-ui, -apple-system, sans-serif; max-width: 46rem; margin: 3rem auto; padding: 0 1.25rem; line-height: 1.6; }
h1 { border-bottom: 1px solid color-mix(in srgb, currentColor 25%, transparent); padding-bottom: .4rem; }
h2 { margin-top: 2.5rem; }
pre { background: color-mix(in srgb, currentColor 8%, transparent); padding: .9rem 1rem; border-radius: .4rem; overflow-x: auto; }
code { background: color-mix(in srgb, currentColor 8%, transparent); padding: .1rem .3rem; border-radius: .2rem; }
pre code { background: none; padding: 0; }
table { border-collapse: collapse; width: 100%; }
th, td { text-align: left; padding: .4rem .6rem .4rem 0; border-bottom: 1px solid color-mix(in srgb, currentColor 15%, transparent); }
td.version { font-family: ui-monospace, monospace; font-size: .9rem; }
.note { font-size: .9rem; opacity: .8; }
</style>
</head>
<body>
<h1>{{.Module}} pre-release registry</h1>

<p>A Bazel registry serving one <code>{{.Module}}</code> version per commit to the
main branch, so a change can be tried out before it is released.
{{if .Repository}}Sources: <a href="{{.Repository}}">{{.Repository}}</a>.{{end}}</p>

<h2>Using it</h2>
<p>Add the registry, keeping the Bazel Central Registry as the fallback for
everything else:</p>
<pre><code>common --registry={{.RegistryURL}} --registry=https://bcr.bazel.build</code></pre>
<p>Then depend on a version from the table below:</p>
<pre><code>bazel_dep(name = "{{.Module}}", version = "{{.Latest}}")</code></pre>

<h2 id="versions">Versions</h2>
<table>
<tr><th>Version</th><th>Built</th><th>Commit</th></tr>
{{- range .Versions}}
<tr>
<td class="version">{{.Version}}</td>
<td>{{.Date}}</td>
<td>{{if .CommitURL}}<a href="{{.CommitURL}}"><code>{{.Commit}}</code></a>{{else if .Commit}}<code>{{.Commit}}</code>{{end}}</td>
</tr>
{{- end}}
</table>

<h2>What you get</h2>
<p class="note">Each version is the module source as of one commit, carrying a
prebuilt lockfile that fetches the <code>img</code> tool from
<code>{{.Artifact}}</code> as an OCI blob, so no tool is built from source.
Versions are pre-releases of the next patch version: they resolve above the last
release and below the next one. Nothing here is a supported release -- a version
may be rewritten by a re-run of the build that produced it, and no version is
ever removed.</p>
</body>
</html>
`))

// writeIndex renders the landing page from the versions a module's metadata
// lists, newest first.
func (w *registryWriter) writeIndex(cfg *Config, versionsNewestFirst []string) error {
	page := indexPage{
		Module:      cfg.Module,
		RegistryURL: cfg.RegistryURL,
		Repository:  w.readMetadataRepository(cfg.Module),
		Artifact:    cfg.Artifact.Registry + "/" + cfg.Artifact.Repository,
	}
	if len(versionsNewestFirst) > 0 {
		page.Latest = versionsNewestFirst[0]
	}
	for _, raw := range versionsNewestFirst {
		entry := indexVersion{Version: raw}
		if stamp := buildStampPattern.FindStringSubmatch(raw); stamp != nil {
			entry.Date = fmt.Sprintf("%s-%s-%s", stamp[1], stamp[2], stamp[3])
			entry.Commit = stamp[4]
			if page.Repository != "" {
				entry.CommitURL = page.Repository + "/commit/" + stamp[4]
			}
		}
		page.Versions = append(page.Versions, entry)
	}

	var rendered strings.Builder
	if err := indexTemplate.Execute(&rendered, page); err != nil {
		return err
	}
	return w.writeFile(indexHTML, []byte(rendered.String()))
}
