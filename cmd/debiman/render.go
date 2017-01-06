package main

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Debian/debiman/internal/manpage"
	"golang.org/x/net/context"
	"golang.org/x/sync/errgroup"
)

//go:generate go run gentmpl.go header footer style manpage manpageerror contents pkgindex index faq

type breadcrumb struct {
	Link string
	Text string
}

var commonTmpls = parseCommonTemplates()

func parseCommonTemplates() *template.Template {
	t := template.New("root")
	t = template.Must(t.New("header").Parse(headerContent))
	t = template.Must(t.New("footer").Parse(footerContent))
	t = template.Must(t.New("style").Parse(styleContent))
	return t
}

func writeAtomically(dest string, write func(w io.Writer) error) error {
	f, err := ioutil.TempFile(filepath.Dir(dest), "debiman-")
	if err != nil {
		return err
	}
	defer f.Close()
	// TODO: defer os.Remove() in case we return before the tempfile is destroyed

	// TODO(later): benchmark/support other compression algorithms. zopfli gets dos2unix from 9659B to 9274B (4% win)

	bufw := bufio.NewWriter(f)

	// NOTE(stapelberg): gzip’s decompression phase takes the same
	// time, regardless of compression level. Hence, we invest the
	// maximum CPU time once to achieve the best compression.
	gzipw, err := gzip.NewWriterLevel(bufw, gzip.BestCompression)
	if err != nil {
		return err
	}
	defer gzipw.Close()

	if err := write(gzipw); err != nil {
		return err
	}

	if err := gzipw.Close(); err != nil {
		return err
	}

	if err := bufw.Flush(); err != nil {
		return err
	}

	if err := f.Chmod(0644); err != nil {
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	return os.Rename(f.Name(), dest)
}

func renderAll(gv globalView) error {
	binsBySuite := make(map[string][]string)

	suitedirs, err := ioutil.ReadDir(*servingDir)
	if err != nil {
		return err
	}
	// To minimize I/O, gather all FileInfos in one pass.
	contents := make(map[string][]os.FileInfo)
	for _, sfi := range suitedirs {
		if !sfi.IsDir() {
			continue
		}
		if !gv.suites[sfi.Name()] {
			continue
		}
		bins, err := ioutil.ReadDir(filepath.Join(*servingDir, sfi.Name()))
		if err != nil {
			return err
		}
		names := make([]string, len(bins))
		for idx, bfi := range bins {
			names[idx] = bfi.Name()
			dir := filepath.Join(*servingDir, sfi.Name(), bfi.Name())
			contents[dir], err = ioutil.ReadDir(dir)
			if err != nil {
				return err
			}
		}
		binsBySuite[sfi.Name()] = names
	}

	// TODO: make render() take a renderJob to avoid duplication
	type renderJob struct {
		dest     string
		src      string
		meta     *manpage.Meta
		versions []*manpage.Meta
		xref     map[string][]*manpage.Meta
	}
	eg, ctx := errgroup.WithContext(context.Background())
	renderChan := make(chan renderJob)
	// TODO: flag for parallelism level
	for i := 0; i < 30; i++ {
		eg.Go(func() error {
			for r := range renderChan {
				if err := rendermanpage(r.dest, r.src, r.meta, r.versions, r.xref); err != nil {
					// render writes an error page if rendering
					// failed, any returned error is severe (e.g. file
					// system full) and should lead to termination.
					return err
				}
			}
			return nil
		})
	}
	var whitelist map[string]bool
	if *onlyRender != "" {
		whitelist = make(map[string]bool)
		log.Printf("Restricting rendering to the following binary packages:")
		for _, e := range strings.Split(strings.TrimSpace(*onlyRender), ",") {
			whitelist[e] = true
			log.Printf("  %q", e)
		}
		log.Printf("(total: %d whitelist entries)", len(whitelist))
	}

	// the invariant is: each file ending in .gz must have a corresponding .html.gz file
	// the .html.gz must have a modtime that is >= the modtime of the .gz file
	for dir, files := range contents {
		if whitelist != nil && !whitelist[filepath.Base(dir)] {
			continue
		}

		fileByName := make(map[string]os.FileInfo, len(files))
		for _, f := range files {
			fileByName[f.Name()] = f
		}

		manpageByName := make(map[string]*manpage.Meta, len(files))

		var indexModTime time.Time
		if fi, ok := fileByName["index.html.gz"]; ok {
			indexModTime = fi.ModTime()
		}
		var indexNeedsUpdate bool

		for _, f := range files {
			full := filepath.Join(dir, f.Name())
			if !strings.HasSuffix(full, ".gz") ||
				strings.HasSuffix(full, ".html.gz") {
				continue
			}
			if f.Mode()&os.ModeSymlink != 0 {
				continue
			}

			if f.ModTime().After(indexModTime) {
				indexNeedsUpdate = true
			}

			m, err := manpage.FromServingPath(*servingDir, full)
			if err != nil {
				// If we run into this case, our code cannot correctly
				// interpret the result of ServingPath().
				log.Printf("BUG: cannot parse manpage from serving path %q: %v", full, err)
				continue
			}

			manpageByName[f.Name()] = m

			n := strings.TrimSuffix(f.Name(), ".gz") + ".html.gz"
			html, ok := fileByName[n]
			if !ok || html.ModTime().Before(f.ModTime()) {
				versions := gv.xref[m.Name]
				// Replace m with its corresponding entry in versions
				// so that render() can use pointer equality to
				// efficiently skip entries.
				for _, v := range versions {
					if v.ServingPath() == m.ServingPath() {
						m = v
						break
					}
				}
				select {
				case renderChan <- renderJob{
					dest:     filepath.Join(dir, n),
					src:      full,
					meta:     m,
					versions: versions,
					xref:     gv.xref,
				}:
				case <-ctx.Done():
					break
				}
			}
		}

		if !indexNeedsUpdate {
			continue
		}

		if err := renderPkgindex(filepath.Join(dir, "index.html.gz"), manpageByName); err != nil {
			return err
		}
	}
	close(renderChan)
	if err := eg.Wait(); err != nil {
		return err
	}

	for suite, bins := range binsBySuite {
		if err := renderContents(filepath.Join(*servingDir, fmt.Sprintf("contents-%s.html.gz", suite)), suite, bins); err != nil {
			return err
		}
	}

	return nil
}