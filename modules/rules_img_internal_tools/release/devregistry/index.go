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
:root {
  color-scheme: light dark;
  --line: color-mix(in srgb, currentColor 15%, transparent);
  --soft: color-mix(in srgb, currentColor 6%, transparent);
  --muted: color-mix(in srgb, currentColor 62%, transparent);
  --mono: ui-monospace, SFMono-Regular, Menlo, monospace;
}
* { box-sizing: border-box; }
body {
  font-family: system-ui, -apple-system, sans-serif;
  max-width: 46rem;
  margin: 0 auto;
  padding: 3.5rem 1.25rem 5rem;
  line-height: 1.6;
}
h1 { font-size: 1.6rem; margin: 0 0 .4rem; }
h2 { font-size: 1.05rem; margin: 2.75rem 0 .75rem; }
p { margin: 0 0 1rem; }
header { border-bottom: 1px solid var(--line); padding-bottom: 1.25rem; }
header p { margin: 0; color: var(--muted); }
a { color: inherit; text-underline-offset: .15em; text-decoration-color: var(--line); }
a:hover { text-decoration-color: currentColor; }
code {
  font-family: var(--mono);
  font-size: .875em;
  background: var(--soft);
  padding: .1em .35em;
  border-radius: .25rem;
}

/* Snippets say which file they belong in, and wrap rather than scroll: a
   registry URL plus a fallback is longer than a line. */
figure { margin: 0 0 1.25rem; border: 1px solid var(--line); border-radius: .5rem; }
figcaption {
  font-family: var(--mono);
  font-size: .8rem;
  color: var(--muted);
  background: var(--soft);
  padding: .4rem .9rem;
  border-bottom: 1px solid var(--line);
  border-radius: .5rem .5rem 0 0;
}
figure pre { margin: 0; padding: .9rem; }
pre code {
  display: block;
  background: none;
  padding: 0 0 0 1.6em;
  font-size: .85rem;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  text-indent: -1.6em;
}

table { border-collapse: collapse; width: 100%; }
th {
  font-size: .75rem;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: .04em;
  color: var(--muted);
  text-align: left;
  padding: 0 .75rem .4rem 0;
  border-bottom: 1px solid var(--line);
}
td { padding: .6rem .75rem; border-bottom: 1px solid var(--line); vertical-align: baseline; }
td:first-child, th:first-child { padding-left: 0; }
td:last-child, th:last-child { padding-right: 0; text-align: right; }
td.version { font-family: var(--mono); font-size: .85rem; overflow-wrap: anywhere; }
td.built { color: var(--muted); white-space: nowrap; }
.latest {
  font-family: system-ui, -apple-system, sans-serif;
  font-size: .7rem;
  text-transform: uppercase;
  letter-spacing: .04em;
  color: var(--muted);
  border: 1px solid var(--line);
  border-radius: 1rem;
  padding: .05rem .45rem;
  margin-left: .45rem;
  white-space: nowrap;
}
.note { color: var(--muted); font-size: .95rem; }
</style>
</head>
<body>
<header>
<h1>{{.Module}} pre-release registry</h1>
<p>Every commit to the main branch is published here as a <code>{{.Module}}</code>
version, so you can use a change before it ships in a release.{{if .Repository}}
<a href="{{.Repository}}">Source</a>.{{end}}</p>
</header>

<h2>Using it</h2>
<p>Add this registry, with the Bazel Central Registry second so that everything
else still resolves:</p>
<figure>
<figcaption>.bazelrc</figcaption>
<pre><code>common --registry={{.RegistryURL}} --registry=https://bcr.bazel.build</code></pre>
</figure>
<p>Then pick a version from the list below:</p>
<figure>
<figcaption>MODULE.bazel</figcaption>
<pre><code>bazel_dep(name = "{{.Module}}", version = "{{.Latest}}")</code></pre>
</figure>

<h2 id="versions">Versions</h2>
<table>
<thead>
<tr><th>Version</th><th>Built</th><th>Commit</th></tr>
</thead>
<tbody>
{{- range $index, $version := .Versions}}
<tr>
<td class="version">{{$version.Version}}{{if eq $index 0}}<span class="latest">latest</span>{{end}}</td>
<td class="built">{{$version.Date}}</td>
<td>{{if $version.CommitURL}}<a href="{{$version.CommitURL}}"><code>{{$version.Commit}}</code></a>{{else if $version.Commit}}<code>{{$version.Commit}}</code>{{end}}</td>
</tr>
{{- end}}
</tbody>
</table>

<h2>What a version is</h2>
<p class="note">The module source at one commit, plus a lockfile that downloads
the <code>img</code> tool from <code>{{.Artifact}}</code> instead of building it.
There is nothing else to set up.</p>
<p class="note">The numbers are pre-releases of the next patch version, so they
sort above the last release and below the next one. These are not releases: a
version can be rewritten if its build is re-run, and none are ever deleted.</p>
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
