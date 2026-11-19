package imagerebuild

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//////////////////////////////////////////////////////////////
// helpers to build source images

type testEntry struct {
	name     string
	typeflag byte
	mode     int64
	uid      int
	gid      int
	uname    string
	gname    string
	modTime  time.Time
	body     string
	linkname string
}

func fileEntry(name, body string) testEntry {
	return testEntry{
		name:     name,
		typeflag: tar.TypeReg,
		mode:     0o644,
		uid:      1000,
		gid:      1000,
		modTime:  time.Unix(1600000000, 0).UTC(),
		body:     body,
	}
}

func execEntry(name, body string) testEntry {
	e := fileEntry(name, body)
	e.mode = 0o755
	return e
}

func dirEntry(name string) testEntry {
	return testEntry{name: name, typeflag: tar.TypeDir, mode: 0o755, uid: 1000, gid: 1000,
		modTime: time.Unix(1600000000, 0).UTC()}
}

func hardlinkEntry(name, target string) testEntry {
	return testEntry{name: name, typeflag: tar.TypeLink, mode: 0o644, uid: 1000, gid: 1000,
		modTime: time.Unix(1600000000, 0).UTC(), linkname: target}
}

func symlinkEntry(name, target string) testEntry {
	return testEntry{name: name, typeflag: tar.TypeSymlink, mode: 0o777, uid: 1000, gid: 1000,
		modTime: time.Unix(1600000000, 0).UTC(), linkname: target}
}

func whiteoutEntry(name string) testEntry {
	return testEntry{name: name, typeflag: tar.TypeReg, mode: 0o644, uid: 0, gid: 0,
		modTime: time.Unix(1600000000, 0).UTC()}
}

func tarBytes(t *testing.T, entries []testEntry) []byte {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     e.mode,
			Uid:      e.uid,
			Gid:      e.gid,
			Uname:    e.uname,
			Gname:    e.gname,
			ModTime:  e.modTime,
			Linkname: e.linkname,
			// PAX keeps sub-second timestamps in the fixture itself.
			Format: tar.FormatPAX,
		}
		if e.typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if hdr.Size > 0 {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

func layerFromEntries(t *testing.T, entries []testEntry) v1.Layer {
	t.Helper()
	layer, err := tarball.LayerFromReader(bytes.NewReader(tarBytes(t, entries)))
	require.NoError(t, err)
	return layer
}

// buildSourceImage assembles a source image from the given layers and config.
// History is filled with one entry per layer so history handling can be
// exercised.
func buildSourceImage(t *testing.T, cfg v1.Config, layers ...v1.Layer) v1.Image {
	t.Helper()

	base, err := mutate.AppendLayers(empty.Image, layers...)
	require.NoError(t, err)

	cf, err := base.ConfigFile()
	require.NoError(t, err)

	cf.Config = cfg
	cf.Created = v1.Time{Time: time.Unix(1600000000, 0).UTC()}
	cf.History = nil
	for i := range layers {
		cf.History = append(cf.History, v1.History{
			Created:    v1.Time{Time: time.Unix(int64(1600000000+i), 0).UTC()},
			CreatedBy:  fmt.Sprintf("step-%d", i),
			Comment:    fmt.Sprintf("layer-%d", i),
			EmptyLayer: false,
		})
	}

	img, err := mutate.ConfigFile(base, cf)
	require.NoError(t, err)
	return img
}

// roundTrip writes the image to a docker-archive tarball and reads it back, so
// the checks run against the serialized form, not against the in-memory image.
func roundTrip(t *testing.T, img v1.Image) v1.Image {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "image.tar")

	ref, err := name.ParseReference("test/image:latest")
	require.NoError(t, err)
	require.NoError(t, tarball.WriteToFile(path, ref, img))

	tag, err := name.NewTag("test/image:latest")
	require.NoError(t, err)
	out, err := tarball.ImageFromPath(path, &tag)
	require.NoError(t, err)
	return out
}

func rebuildToImage(t *testing.T, src v1.Image, opts Options) (v1.Image, *Result) {
	t.Helper()

	res, err := Rebuild(src, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = res.Close() })

	return roundTrip(t, res.Image), res
}

//////////////////////////////////////////////////////////////
// filesystem view extraction used as the reference for comparisons

type viewNode struct {
	kind     string // file, dir, symlink, other
	mode     os.FileMode
	uid      int
	gid      int
	modTime  time.Time
	linkname string
	sum      string
}

func extractView(t *testing.T, img v1.Image) (map[string]*viewNode, map[string]os.FileInfo) {
	t.Helper()

	root := t.TempDir()
	layers, err := img.Layers()
	require.NoError(t, err)

	dirTimes := map[string]time.Time{}

	for _, layer := range layers {
		rc, err := layer.Uncompressed()
		require.NoError(t, err)

		// An opaque marker hides the lower layers, not the entries this layer
		// already unpacked.
		thisLayer := map[string]bool{}

		tr := tar.NewReader(rc)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)

			base := filepath.Base(hdr.Name)
			target := filepath.Join(root, hdr.Name)

			switch {
			case base == ".wh..wh..opq" || base == ".wh..opq":
				dir := filepath.Dir(target)
				children, err := os.ReadDir(dir)
				if err == nil {
					for _, c := range children {
						childPath := filepath.Join(dir, c.Name())
						if thisLayer[childPath] {
							continue
						}
						require.NoError(t, os.RemoveAll(childPath))
					}
				}
				continue
			case len(base) > len(".wh.") && base[:len(".wh.")] == ".wh.":
				require.NoError(t, os.RemoveAll(filepath.Join(filepath.Dir(target), base[len(".wh."):])))
				continue
			}

			thisLayer[target] = true

			switch hdr.Typeflag {
			case tar.TypeDir:
				require.NoError(t, os.MkdirAll(target, 0o755))
				dirTimes[target] = hdr.ModTime
			case tar.TypeReg:
				require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
				require.NoError(t, os.RemoveAll(target))
				f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
				require.NoError(t, err)
				_, err = io.Copy(f, tr)
				require.NoError(t, err)
				require.NoError(t, f.Close())
				require.NoError(t, os.Chmod(target, os.FileMode(hdr.Mode)))
				require.NoError(t, os.Chtimes(target, hdr.ModTime, hdr.ModTime))
			case tar.TypeSymlink:
				require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
				require.NoError(t, os.RemoveAll(target))
				require.NoError(t, os.Symlink(hdr.Linkname, target))
			case tar.TypeLink:
				require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
				require.NoError(t, os.RemoveAll(target))
				require.NoError(t, os.Link(filepath.Join(root, hdr.Linkname), target))
			default:
				t.Fatalf("unsupported entry type %d (%s)", hdr.Typeflag, hdr.Name)
			}
		}
		require.NoError(t, rc.Close())
	}

	view := map[string]*viewNode{}
	inodes := map[string]os.FileInfo{}

	err = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		key := "/" + filepath.ToSlash(rel)

		li, err := os.Lstat(p)
		require.NoError(t, err)

		node := &viewNode{
			mode: li.Mode(),
			sum:  "",
		}

		switch {
		case li.Mode()&os.ModeSymlink != 0:
			node.kind = "symlink"
			dest, err := os.Readlink(p)
			require.NoError(t, err)
			node.linkname = dest
		case li.IsDir():
			node.kind = "dir"
			// Only directories that carry an explicit header from the image are
			// compared; implicit parents have no image-side timestamp.
			if mt, ok := dirTimes[p]; ok {
				node.modTime = mt.UTC()
			}
		case li.Mode().IsRegular():
			node.kind = "file"
			node.modTime = li.ModTime().UTC()
			data, err := os.ReadFile(p)
			require.NoError(t, err)
			s := sha256.Sum256(data)
			node.sum = hex.EncodeToString(s[:])
			inodes[key] = li
		default:
			node.kind = "other"
			node.modTime = li.ModTime().UTC()
		}

		view[key] = node
		return nil
	})
	require.NoError(t, err)

	// Directory mtimes are restored at the end, after all children exist. A
	// directory can be gone by now (removed by a later whiteout).
	for p, mt := range dirTimes {
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		require.NoError(t, os.Chtimes(p, mt, mt))
	}

	return view, inodes
}

func assertViewsEqual(t *testing.T, want, got map[string]*viewNode) {
	t.Helper()

	wantKeys := sortedKeys(want)
	gotKeys := sortedKeys(got)
	require.Equal(t, wantKeys, gotKeys, "final view path sets differ")

	for _, p := range wantKeys {
		w := want[p]
		g := got[p]
		assert.Equal(t, w.kind, g.kind, "kind mismatch for %s", p)
		assert.Equal(t, w.mode, g.mode, "mode mismatch for %s", p)
		assert.Equal(t, w.modTime, g.modTime, "mtime mismatch for %s", p)
		assert.Equal(t, w.linkname, g.linkname, "link target mismatch for %s", p)
		assert.Equal(t, w.sum, g.sum, "content mismatch for %s", p)
	}
}

func sortedKeys(m map[string]*viewNode) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// viewHeader is the archive-level description of a file in the final view.
// Unlike viewNode it also carries ownership, which cannot be applied to files
// in a test process running as an unprivileged user.
type viewHeader struct {
	typeflag byte
	mode     int64
	uid      int
	gid      int
	uname    string
	gname    string
	modTime  time.Time
	linkname string
	size     int64
	sum      string
}

// finalViewHeaders computes the final view straight from the layer headers.
// It resolves hard links to a concrete data entry so both the source and the
// rebuilt image can be compared uniformly.
func finalViewHeaders(t *testing.T, img v1.Image) map[string]*viewHeader {
	t.Helper()

	state := map[string]*viewHeader{}
	content := map[string]*viewHeader{}
	// meta holds the aufs hard link store; it is not part of the view but the
	// links that point into it need to be resolved.
	meta := map[string]*viewHeader{}

	removeSubtree := func(target string) {
		delete(state, target)
		delete(content, target)
		prefix := target
		if prefix != "/" {
			prefix += "/"
		}
		for p := range state {
			if len(p) > len(prefix) && p[:len(prefix)] == prefix {
				delete(state, p)
				delete(content, p)
			}
		}
	}

	// resolve follows a chain of hard links to the entry that holds the data.
	resolve := func(linkPath string) *viewHeader {
		for i := 0; i < 100; i++ {
			h := state[linkPath]
			if h == nil {
				h = meta[linkPath]
			}
			if h == nil || h.typeflag != tar.TypeLink {
				return h
			}
			linkPath = normalizePath(h.linkname)
		}
		return nil
	}

	layers, err := img.Layers()
	require.NoError(t, err)

	for _, layer := range layers {
		rc, err := layer.Uncompressed()
		require.NoError(t, err)

		// Paths written by this layer; an opaque marker hides the lower layers
		// but not entries from its own layer.
		thisLayer := map[string]bool{}

		tr := tar.NewReader(rc)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)

			name := normalizePath(hdr.Name)
			base := path.Base(name)
			dir := path.Dir(name)

			switch {
			case base == ".wh..wh..opq" || base == ".wh..opq":
				prefix := dir
				if prefix != "/" {
					prefix += "/"
				}
				for p := range state {
					if len(p) > len(prefix) && p[:len(prefix)] == prefix && !thisLayer[p] {
						delete(state, p)
						delete(content, p)
					}
				}
				continue
			case strings.HasPrefix(name, "/.wh..wh.") && hdr.Typeflag == tar.TypeReg:
				// Aufs metadata (including the hard link store) is not part of
				// the view, but link entries resolve against it.
				h := &viewHeader{
					typeflag: hdr.Typeflag,
					mode:     hdr.Mode,
					uid:      hdr.Uid,
					gid:      hdr.Gid,
					uname:    hdr.Uname,
					gname:    hdr.Gname,
					modTime:  hdr.ModTime.UTC(),
					size:     hdr.Size,
				}
				data, err := io.ReadAll(tr)
				require.NoError(t, err)
				sum := sha256.Sum256(data)
				h.sum = hex.EncodeToString(sum[:])
				meta[name] = h
				continue
			case strings.HasPrefix(name, "/.wh..wh."):
				// Aufs metadata directories are never part of the view.
				continue
			case len(base) > 4 && base[:4] == ".wh.":
				removeSubtree(path.Join(dir, base[4:]))
				continue
			}

			thisLayer[name] = true

			h := &viewHeader{
				typeflag: hdr.Typeflag,
				mode:     hdr.Mode,
				uid:      hdr.Uid,
				gid:      hdr.Gid,
				uname:    hdr.Uname,
				gname:    hdr.Gname,
				modTime:  hdr.ModTime.UTC(),
				linkname: hdr.Linkname,
				size:     hdr.Size,
			}

			// Directories merge with what lower layers put at the same path;
			// any other entry replaces the path. A directory written over a
			// non-directory replaces it.
			if hdr.Typeflag == tar.TypeDir {
				if prev, ok := state[name]; ok && prev.typeflag != tar.TypeDir {
					delete(state, name)
					delete(content, name)
				}
			} else {
				removeSubtree(name)
			}
			state[name] = h

			switch hdr.Typeflag {
			case tar.TypeReg:
				data, err := io.ReadAll(tr)
				require.NoError(t, err)
				sum := sha256.Sum256(data)
				h.sum = hex.EncodeToString(sum[:])
				content[name] = h
			case tar.TypeLink:
				target := resolve(normalizePath(hdr.Linkname))
				if target != nil {
					h.sum = target.sum
					content[name] = target
					h.size = target.size
				}
			}
		}
		require.NoError(t, rc.Close())
	}

	// Fold hard links into their data entries so kinds compare directly.
	out := map[string]*viewHeader{}
	for p, h := range state {
		if h.typeflag == tar.TypeLink {
			cp := *h
			if c := content[p]; c != nil {
				cp.sum = c.sum
				cp.size = c.size
			}
			out[p] = &cp
			continue
		}
		out[p] = h
	}
	return out
}

func assertHeadersEqual(t *testing.T, want, got map[string]*viewHeader) {
	t.Helper()

	wantKeys := make([]string, 0, len(want))
	for k := range want {
		wantKeys = append(wantKeys, k)
	}
	gotKeys := make([]string, 0, len(got))
	for k := range got {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(wantKeys)
	sort.Strings(gotKeys)
	require.Equal(t, wantKeys, gotKeys)

	for _, p := range wantKeys {
		w := want[p]
		g := got[p]

		// A hard link whose partner is gone becomes a regular file; the
		// remaining name must then describe the same data.
		if w.typeflag == tar.TypeLink && g.typeflag == tar.TypeReg {
			assert.Equal(t, w.sum, g.sum, "materialized hard link content for %s", p)
			continue
		}

		assert.Equal(t, w.typeflag, g.typeflag, "type for %s", p)
		assert.Equal(t, w.mode, g.mode, "mode for %s", p)
		assert.Equal(t, w.uid, g.uid, "uid for %s", p)
		assert.Equal(t, w.gid, g.gid, "gid for %s", p)
		assert.Equal(t, w.uname, g.uname, "uname for %s", p)
		assert.Equal(t, w.gname, g.gname, "gname for %s", p)
		assert.Equal(t, w.modTime, g.modTime, "mtime for %s", p)
		assert.Equal(t, w.linkname, g.linkname, "link target for %s", p)
		assert.Equal(t, w.size, g.size, "size for %s", p)
		assert.Equal(t, w.sum, g.sum, "content for %s", p)
	}
}

func assertNoWhiteouts(t *testing.T, img v1.Image) {
	t.Helper()

	layers, err := img.Layers()
	require.NoError(t, err)
	for _, layer := range layers {
		rc, err := layer.Uncompressed()
		require.NoError(t, err)
		tr := tar.NewReader(rc)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			assert.NotContains(t, filepath.Base(hdr.Name), ".wh.", "unexpected whiteout entry %q", hdr.Name)
		}
		require.NoError(t, rc.Close())
	}
}

func preservedConfigBytes(t *testing.T, img v1.Image) []byte {
	t.Helper()

	cf, err := img.ConfigFile()
	require.NoError(t, err)

	cf = cf.DeepCopy()
	cf.RootFS.DiffIDs = nil
	cf.History = nil

	data, err := json.Marshal(cf)
	require.NoError(t, err)
	return data
}

//////////////////////////////////////////////////////////////
// fixture

var testConfig = v1.Config{
	User:         "1001:1001",
	ExposedPorts: map[string]struct{}{"8080/tcp": {}, "9090/udp": {}},
	Env:          []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "PATH=/opt/bin:/usr/bin:/bin"},
	Entrypoint:   []string{"/bin/app", "--serve"},
	Cmd:          []string{"--port", "8080"},
	Volumes:      map[string]struct{}{"/data": {}, "/logs": {}},
	WorkingDir:   "/srv/app",
	Labels:       map[string]string{"maintainer": "test", "version": "1.0"},
	StopSignal:   "SIGTERM",
	Shell:        []string{"/bin/sh", "-c"},
	Healthcheck: &v1.HealthConfig{
		Test:        []string{"CMD-SHELL", "curl -f http://localhost:8080/ || exit 1"},
		Interval:    30 * time.Second,
		Timeout:     5 * time.Second,
		StartPeriod: 10 * time.Second,
		Retries:     3,
	},
}

// fixtureLayers returns the three layers described in the task:
//   - layer 2 overrides a file from layer 1,
//   - layer 3 whiteouts a file that layer 2 added,
//   - a hard link pair and a symlink are part of the image.
func fixtureLayers(t *testing.T, dropHardLink bool) []v1.Layer {
	t.Helper()

	layer1 := []testEntry{
		dirEntry("etc/"),
		fileEntry("etc/app.conf", "config-v1\n"),
		fileEntry("etc/keep1.txt", "keep-1\n"),
		execEntry("bin/app", "#!/bin/sh\necho v1\n"),
		fileEntry("data/shared.bin", "shared-content\n"),
		hardlinkEntry("data/shared.link", "data/shared.bin"),
		symlinkEntry("data/keep1.symlink", "keep1.txt"),
		dirEntry("var/"),
		dirEntry("var/cache/"),
	}

	layer2 := []testEntry{
		fileEntry("etc/app.conf", "config-v2\n"),
		fileEntry("etc/keep2.txt", "keep-2\n"),
		fileEntry("var/cache/blob.bin", "cache-blob\n"),
	}

	layer3 := []testEntry{
		whiteoutEntry("etc/.wh.keep2.txt"),
		dirEntry("var/"),
		whiteoutEntry("var/.wh..wh..opq"),
		fileEntry("var/run.txt", "runtime\n"),
	}

	if dropHardLink {
		// Only one name of the hard link pair survives; the content must move
		// to the remaining name.
		layer3 = append(layer3, whiteoutEntry("data/.wh.shared.bin"))
	}

	return []v1.Layer{
		layerFromEntries(t, layer1),
		layerFromEntries(t, layer2),
		layerFromEntries(t, layer3),
	}
}

//////////////////////////////////////////////////////////////
// tests

func TestRebuildPreservesFinalView(t *testing.T) {
	for _, dropHardLink := range []bool{false, true} {
		for _, mode := range []Mode{ModePreserveLayers, ModeSingleLayer} {
			name := fmt.Sprintf("mode=%s/dropHardLink=%v", mode, dropHardLink)
			t.Run(name, func(t *testing.T) {
				src := buildSourceImage(t, testConfig, fixtureLayers(t, dropHardLink)...)

				wantView, _ := extractView(t, src)

				out, res := rebuildToImage(t, src, Options{Mode: mode})
				gotView, _ := extractView(t, out)

				if dropHardLink {
					assert.NotContains(t, wantView, "/data/shared.bin")
					assert.Contains(t, wantView, "/data/shared.link")
					assert.Equal(t, hexOf("shared-content\n"), wantView["/data/shared.link"].sum,
						"hard link content must survive under the remaining name")
				} else {
					assert.Contains(t, wantView, "/data/shared.bin")
					assert.Contains(t, wantView, "/data/shared.link")
					assert.Equal(t, wantView["/data/shared.bin"].sum, wantView["/data/shared.link"].sum)
				}

				assertViewsEqual(t, wantView, gotView)

				// The overridden file must come from the winning layer.
				require.Contains(t, gotView, "/etc/app.conf")
				assert.Equal(t, hexOf("config-v2\n"), gotView["/etc/app.conf"].sum)

				// The whiteouted file must not come back.
				assert.NotContains(t, gotView, "/etc/keep2.txt")
				assert.NotContains(t, gotView, "/var/cache/blob.bin",
					"opaque directory must hide lower-layer children")

				// No whiteout markers survive into the rebuilt image.
				assertNoWhiteouts(t, out)

				switch mode {
				case ModePreserveLayers:
					// All three layers still contribute at least one file: the
					// whiteout-only layer is the only candidate for dropping.
					assert.Equal(t, 3, res.RetainedLayers)
					assert.Equal(t, 0, res.DroppedLayers)
				case ModeSingleLayer:
					assert.Equal(t, 1, res.RetainedLayers)
				}
			})
		}
	}
}

func TestRebuildKeepsHardLinkRelationship(t *testing.T) {
	src := buildSourceImage(t, testConfig, fixtureLayers(t, false)...)

	_, srcInodes := extractView(t, src)
	require.True(t, os.SameFile(srcInodes["/data/shared.bin"], srcInodes["/data/shared.link"]),
		"fixture must contain a real hard link pair")

	for _, mode := range []Mode{ModePreserveLayers, ModeSingleLayer} {
		t.Run(string(mode), func(t *testing.T) {
			out, _ := rebuildToImage(t, src, Options{Mode: mode})
			_, inodes := extractView(t, out)
			require.Contains(t, inodes, "/data/shared.bin")
			require.Contains(t, inodes, "/data/shared.link")
			assert.True(t, os.SameFile(inodes["/data/shared.bin"], inodes["/data/shared.link"]),
				"both hard link names are retained, so they must stay linked")
		})
	}
}

func TestRebuildMaterializesRemainingHardLinkName(t *testing.T) {
	src := buildSourceImage(t, testConfig, fixtureLayers(t, true)...)

	want, _ := extractView(t, src)
	assert.Equal(t, "file", want["/data/shared.link"].kind)

	for _, mode := range []Mode{ModePreserveLayers, ModeSingleLayer} {
		t.Run(string(mode), func(t *testing.T) {
			out, _ := rebuildToImage(t, src, Options{Mode: mode})
			got, _ := extractView(t, out)

			require.Contains(t, got, "/data/shared.link")
			assert.Equal(t, "file", got["/data/shared.link"].kind,
				"the remaining name must be a regular file once the other name is gone")
			assert.Equal(t, hexOf("shared-content\n"), got["/data/shared.link"].sum)
			assert.NotContains(t, got, "/data/shared.bin")
		})
	}
}

func TestRebuildIsDeterministic(t *testing.T) {
	src := buildSourceImage(t, testConfig, fixtureLayers(t, false)...)

	for _, mode := range []Mode{ModePreserveLayers, ModeSingleLayer} {
		t.Run(string(mode), func(t *testing.T) {
			first, _ := rebuildToImage(t, src, Options{Mode: mode})
			second, _ := rebuildToImage(t, src, Options{Mode: mode})

			firstDigest, err := first.Digest()
			require.NoError(t, err)
			secondDigest, err := second.Digest()
			require.NoError(t, err)
			assert.Equal(t, firstDigest.String(), secondDigest.String())

			firstID, err := first.ConfigName()
			require.NoError(t, err)
			secondID, err := second.ConfigName()
			require.NoError(t, err)
			assert.Equal(t, firstID.String(), secondID.String())
		})
	}
}

func TestRebuildSucceedsWithoutWallClockInfluence(t *testing.T) {
	// A source image with zeroed timestamps must stay zeroed: the rebuild may
	// not inject the current time into headers or the config.
	src := buildSourceImage(t, testConfig, fixtureLayers(t, false)...)

	out, _ := rebuildToImage(t, src, Options{})

	cf, err := out.ConfigFile()
	require.NoError(t, err)
	assert.Equal(t, time.Unix(1600000000, 0).UTC(), cf.Created.UTC())

	var maxHeaderTime time.Time
	layers, err := out.Layers()
	require.NoError(t, err)
	for _, layer := range layers {
		rc, err := layer.Uncompressed()
		require.NoError(t, err)
		tr := tar.NewReader(rc)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			if hdr.ModTime.After(maxHeaderTime) {
				maxHeaderTime = hdr.ModTime
			}
		}
		require.NoError(t, rc.Close())
	}
	assert.True(t, maxHeaderTime.Before(time.Now().Add(-24*time.Hour)),
		"header timestamps must come from the source image")
}

func TestRebuildPreservesSubSecondTimestamps(t *testing.T) {
	entries := []testEntry{
		fileEntry("etc/sub.txt", "sub-second\n"),
	}
	entries[0].modTime = time.Unix(1600000000, 123456789).UTC()

	src := buildSourceImage(t, testConfig, layerFromEntries(t, entries))

	out, _ := rebuildToImage(t, src, Options{})

	want, _ := extractView(t, src)
	got, _ := extractView(t, out)
	assert.Equal(t, want["/etc/sub.txt"].modTime, got["/etc/sub.txt"].modTime)
	assert.Equal(t, 123456789, got["/etc/sub.txt"].modTime.Nanosecond())
}

func TestRebuildHistoryModes(t *testing.T) {
	src := buildSourceImage(t, testConfig, fixtureLayers(t, false)...)

	t.Run("dropped by default", func(t *testing.T) {
		out, res := rebuildToImage(t, src, Options{})
		cf, err := out.ConfigFile()
		require.NoError(t, err)
		assert.Empty(t, cf.History)
		assert.False(t, res.HistoryKept)
	})

	t.Run("kept for retained layers", func(t *testing.T) {
		out, res := rebuildToImage(t, src, Options{KeepHistory: true})
		cf, err := out.ConfigFile()
		require.NoError(t, err)
		require.Len(t, cf.History, 3)
		assert.Equal(t, "step-0", cf.History[0].CreatedBy)
		assert.Equal(t, "step-1", cf.History[1].CreatedBy)
		assert.Equal(t, "step-2", cf.History[2].CreatedBy)
		assert.True(t, res.HistoryKept)
	})

	t.Run("entries of dropped layers are dropped", func(t *testing.T) {
		// Layer 2 only carries a whiteout, so it is dropped together with its
		// history entry.
		emptyable := buildSourceImage(t, testConfig,
			layerFromEntries(t, []testEntry{
				dirEntry("etc/"),
				fileEntry("etc/tmp.txt", "tmp\n"),
			}),
			layerFromEntries(t, []testEntry{whiteoutEntry("etc/.wh.tmp.txt")}),
		)

		out, res := rebuildToImage(t, emptyable, Options{KeepHistory: true})
		assert.Equal(t, 1, res.RetainedLayers)
		assert.Equal(t, 1, res.DroppedLayers)

		cf, err := out.ConfigFile()
		require.NoError(t, err)
		require.Len(t, cf.History, 1)
		assert.Equal(t, "step-0", cf.History[0].CreatedBy)
	})

	t.Run("collapsed when single layer", func(t *testing.T) {
		out, _ := rebuildToImage(t, src, Options{Mode: ModeSingleLayer, KeepHistory: true})
		cf, err := out.ConfigFile()
		require.NoError(t, err)
		assert.Empty(t, cf.History)
	})
}

func TestRebuildRejectsUnknownMode(t *testing.T) {
	src := buildSourceImage(t, testConfig, fixtureLayers(t, false)...)
	_, err := Rebuild(src, Options{Mode: "bogus"})
	require.Error(t, err)
}

func TestRebuildEmptyImage(t *testing.T) {
	// Layers whose only content is whiteouts produce an image with no layers.
	src := buildSourceImage(t, testConfig,
		layerFromEntries(t, []testEntry{fileEntry("etc/tmp.txt", "x")}),
		layerFromEntries(t, []testEntry{whiteoutEntry("etc/.wh.tmp.txt")}),
	)

	out, res := rebuildToImage(t, src, Options{})
	assert.Equal(t, 0, res.RetainedLayers)

	digest, err := out.Digest()
	require.NoError(t, err)
	assert.NotEmpty(t, digest.String())

	again, _ := rebuildToImage(t, src, Options{})
	againDigest, err := again.Digest()
	require.NoError(t, err)
	assert.Equal(t, digest.String(), againDigest.String())
}

func TestRebuildPreservesOCIMediaTypes(t *testing.T) {
	src := buildSourceImage(t, testConfig, fixtureLayers(t, false)...)
	src = mutate.MediaType(src, types.OCIManifestSchema1)
	src = mutate.ConfigMediaType(src, types.OCIConfigJSON)

	// The docker-archive round trip normalizes media types, so this check runs
	// against the in-memory image.
	res, err := Rebuild(src, Options{})
	require.NoError(t, err)
	defer res.Close()

	mt, err := res.Image.MediaType()
	require.NoError(t, err)
	assert.Equal(t, types.OCIManifestSchema1, mt)

	layers, err := res.Image.Layers()
	require.NoError(t, err)
	require.NotEmpty(t, layers)
	for _, layer := range layers {
		lmt, err := layer.MediaType()
		require.NoError(t, err)
		assert.Equal(t, types.OCILayer, lmt)
	}

	manifest, err := res.Image.Manifest()
	require.NoError(t, err)
	assert.Equal(t, types.OCIConfigJSON, manifest.Config.MediaType)
}

func TestRebuildPreservesHeaders(t *testing.T) {
	layers := []v1.Layer{
		layerFromEntries(t, []testEntry{
			dirEntry("opt/"),
			{
				name:     "opt/tool",
				typeflag: tar.TypeReg,
				mode:     0o4755, // setuid
				uid:      0,
				gid:      0,
				uname:    "root",
				gname:    "root",
				modTime:  time.Unix(1500000000, 500000000).UTC(),
				body:     "binary",
			},
			{
				name:     "opt/secret",
				typeflag: tar.TypeReg,
				mode:     0o600,
				uid:      4242,
				gid:      4343,
				uname:    "appuser",
				gname:    "appgroup",
				modTime:  time.Unix(1500000001, 0).UTC(),
				body:     "secret",
			},
			{
				name:     "opt/link",
				typeflag: tar.TypeSymlink,
				mode:     0o777,
				uid:      4242,
				gid:      4343,
				modTime:  time.Unix(1500000002, 0).UTC(),
				linkname: "tool",
			},
		}),
	}

	src := buildSourceImage(t, testConfig, layers...)
	want := finalViewHeaders(t, src)

	for _, mode := range []Mode{ModePreserveLayers, ModeSingleLayer} {
		t.Run(string(mode), func(t *testing.T) {
			out, _ := rebuildToImage(t, src, Options{Mode: mode})
			got := finalViewHeaders(t, out)
			assertHeadersEqual(t, want, got)
		})
	}
}

func TestRebuildOpaqueDirKeepsSameLayerEntries(t *testing.T) {
	// An opaque marker hides lower-layer children but not entries from its own
	// layer, whether they were written before or after the marker.
	src := buildSourceImage(t, testConfig,
		layerFromEntries(t, []testEntry{
			dirEntry("var/"),
			fileEntry("var/old.txt", "old"),
			dirEntry("var/sub/"),
			fileEntry("var/sub/old.txt", "old-sub"),
		}),
		layerFromEntries(t, []testEntry{
			fileEntry("var/new-before.txt", "new-before"),
			dirEntry("var/"),
			whiteoutEntry("var/.wh..wh..opq"),
			fileEntry("var/new-after.txt", "new-after"),
		}),
	)

	want := finalViewHeaders(t, src)
	require.Contains(t, want, "/var/new-before.txt")
	require.Contains(t, want, "/var/new-after.txt")
	require.NotContains(t, want, "/var/old.txt")
	require.NotContains(t, want, "/var/sub")

	for _, mode := range []Mode{ModePreserveLayers, ModeSingleLayer} {
		t.Run(string(mode), func(t *testing.T) {
			out, _ := rebuildToImage(t, src, Options{Mode: mode})
			got := finalViewHeaders(t, out)
			assertHeadersEqual(t, want, got)
		})
	}
}

func TestRebuildDirectoryOverwrittenByFile(t *testing.T) {
	src := buildSourceImage(t, testConfig,
		layerFromEntries(t, []testEntry{
			fileEntry("srv/thing/inner.txt", "inner"),
		}),
		layerFromEntries(t, []testEntry{
			fileEntry("srv/thing", "now-a-file"),
		}),
	)

	want := finalViewHeaders(t, src)
	require.Equal(t, byte(tar.TypeReg), want["/srv/thing"].typeflag)
	require.NotContains(t, want, "/srv/thing/inner.txt")

	for _, mode := range []Mode{ModePreserveLayers, ModeSingleLayer} {
		t.Run(string(mode), func(t *testing.T) {
			out, _ := rebuildToImage(t, src, Options{Mode: mode})
			got := finalViewHeaders(t, out)
			assertHeadersEqual(t, want, got)
		})
	}
}

func TestRebuildSymlinkIsNotFollowed(t *testing.T) {
	src := buildSourceImage(t, testConfig,
		layerFromEntries(t, []testEntry{
			fileEntry("opt/real.txt", "real"),
			symlinkEntry("opt/alias", "real.txt"),
			// A whiteout that names the symlink must not remove the target.
			fileEntry("opt/other.txt", "other"),
		}),
		layerFromEntries(t, []testEntry{
			whiteoutEntry("opt/.wh.alias"),
		}),
	)

	want := finalViewHeaders(t, src)
	require.NotContains(t, want, "/opt/alias")
	require.Contains(t, want, "/opt/real.txt")

	out, _ := rebuildToImage(t, src, Options{})
	got := finalViewHeaders(t, out)
	assertHeadersEqual(t, want, got)
}

func TestRebuildChainedHardLinks(t *testing.T) {
	src := buildSourceImage(t, testConfig,
		layerFromEntries(t, []testEntry{
			fileEntry("data/base.bin", "base"),
			hardlinkEntry("data/mid.bin", "data/base.bin"),
			hardlinkEntry("data/tip.bin", "data/mid.bin"),
		}),
	)

	for _, mode := range []Mode{ModePreserveLayers, ModeSingleLayer} {
		t.Run(string(mode)+"/all kept", func(t *testing.T) {
			out, _ := rebuildToImage(t, src, Options{Mode: mode})
			want := finalViewHeaders(t, src)
			got := finalViewHeaders(t, out)
			assertHeadersEqual(t, want, got)

			_, inodes := extractView(t, out)
			require.Len(t, inodes, 3)
			assert.True(t, os.SameFile(inodes["/data/base.bin"], inodes["/data/mid.bin"]))
			assert.True(t, os.SameFile(inodes["/data/base.bin"], inodes["/data/tip.bin"]))
		})
	}

	// Removing the head of the chain: the middle link becomes a regular file,
	// and the tip may stay a hard link to the materialized middle name.
	trimmed := buildSourceImage(t, testConfig,
		layerFromEntries(t, []testEntry{
			fileEntry("data/base.bin", "base"),
			hardlinkEntry("data/mid.bin", "data/base.bin"),
			hardlinkEntry("data/tip.bin", "data/mid.bin"),
		}),
		layerFromEntries(t, []testEntry{
			whiteoutEntry("data/.wh.base.bin"),
		}),
	)

	for _, mode := range []Mode{ModePreserveLayers, ModeSingleLayer} {
		t.Run(string(mode)+"/head removed", func(t *testing.T) {
			out, _ := rebuildToImage(t, trimmed, Options{Mode: mode})
			want := finalViewHeaders(t, trimmed)
			got := finalViewHeaders(t, out)
			assertHeadersEqual(t, want, got)

			assert.NotContains(t, got, "/data/base.bin")
			assert.Equal(t, hexOf("base"), got["/data/mid.bin"].sum)
			assert.Equal(t, hexOf("base"), got["/data/tip.bin"].sum)
		})
	}
}

func TestRebuildHardLinkTargetReplacedLater(t *testing.T) {
	// The file the link points at is replaced by a later layer. The link keeps
	// the original inode's content, so it must not be re-pointed at the new
	// content.
	src := buildSourceImage(t, testConfig,
		layerFromEntries(t, []testEntry{
			fileEntry("data/orig.txt", "original"),
			hardlinkEntry("data/alias.txt", "data/orig.txt"),
		}),
		layerFromEntries(t, []testEntry{
			fileEntry("data/orig.txt", "replaced"),
		}),
	)

	want := finalViewHeaders(t, src)
	require.Equal(t, hexOf("original"), want["/data/alias.txt"].sum)
	require.Equal(t, hexOf("replaced"), want["/data/orig.txt"].sum)

	for _, mode := range []Mode{ModePreserveLayers, ModeSingleLayer} {
		t.Run(string(mode), func(t *testing.T) {
			out, _ := rebuildToImage(t, src, Options{Mode: mode})
			got := finalViewHeaders(t, out)
			assertHeadersEqual(t, want, got)
			assert.Equal(t, hexOf("original"), got["/data/alias.txt"].sum)
			assert.Equal(t, hexOf("replaced"), got["/data/orig.txt"].sum)
		})
	}
}

func TestRebuildPreserveLayersReusesUntouchedLayers(t *testing.T) {
	// Layer 1 is untouched, layer 2 overrides a file, layer 3 only deletes.
	// The untouched layer must keep its digest so the slimmed image keeps
	// sharing it with the base image.
	src := buildSourceImage(t, testConfig,
		layerFromEntries(t, []testEntry{
			dirEntry("opt/"),
			fileEntry("opt/keep.txt", "keep"),
		}),
		layerFromEntries(t, []testEntry{
			fileEntry("opt/over.txt", "v1"),
		}),
		layerFromEntries(t, []testEntry{
			fileEntry("opt/over.txt", "v2"),
			fileEntry("opt/temp.txt", "temp"),
		}),
		layerFromEntries(t, []testEntry{
			whiteoutEntry("opt/.wh.temp.txt"),
		}),
	)

	srcLayers, err := src.Layers()
	require.NoError(t, err)

	out, res := rebuildToImage(t, src, Options{Mode: ModePreserveLayers})
	outLayers, err := out.Layers()
	require.NoError(t, err)
	// Layer 1 is untouched (reused), layer 3 keeps only the surviving file,
	// while layers 2 and 4 end up empty and are dropped.
	require.Len(t, outLayers, 2)

	// The round trip through a docker archive may re-compress, so compare the
	// in-memory result for the digest check.
	res2, err := Rebuild(src, Options{Mode: ModePreserveLayers})
	require.NoError(t, err)
	defer res2.Close()

	assert.Equal(t, 1, res2.ReusedLayers)
	assert.Equal(t, 2, res.DroppedLayers)

	memLayers, err := res2.Image.Layers()
	require.NoError(t, err)
	require.Len(t, memLayers, 2)

	srcDigest, err := srcLayers[0].Digest()
	require.NoError(t, err)
	memDigest, err := memLayers[0].Digest()
	require.NoError(t, err)
	assert.Equal(t, srcDigest.String(), memDigest.String(),
		"the untouched layer must keep its digest")

	want := finalViewHeaders(t, src)
	got := finalViewHeaders(t, out)
	assertHeadersEqual(t, want, got)

	// Single layer mode never reuses source blobs.
	_, single := rebuildToImage(t, src, Options{Mode: ModeSingleLayer})
	assert.Zero(t, single.ReusedLayers)
}

func TestRebuildMaterializesAufsHardLinkStore(t *testing.T) {
	// Some producers (aufs) stash hard link data under .wh..wh.plnk and emit
	// typeflag Link entries that point into it. The store is metadata, not part
	// of the view, but the link must keep its content.
	src := buildSourceImage(t, testConfig,
		layerFromEntries(t, []testEntry{
			dirEntry(".wh..wh.plnk/"),
			dirEntry(".wh..wh.plnk/42/"),
			fileEntry(".wh..wh.plnk/42/file1", "aufs-content"),
			hardlinkEntry("usr/bin/tool", ".wh..wh.plnk/42/file1"),
		}),
	)

	want := finalViewHeaders(t, src)
	assert.NotContains(t, want, "/.wh..wh.plnk/42/file1")
	require.Contains(t, want, "/usr/bin/tool")
	assert.Equal(t, hexOf("aufs-content"), want["/usr/bin/tool"].sum)

	for _, mode := range []Mode{ModePreserveLayers, ModeSingleLayer} {
		t.Run(string(mode), func(t *testing.T) {
			out, _ := rebuildToImage(t, src, Options{Mode: mode})
			got := finalViewHeaders(t, out)
			assertHeadersEqual(t, want, got)
			assert.Equal(t, hexOf("aufs-content"), got["/usr/bin/tool"].sum)
			assert.NoDirExists(t, "/.wh..wh.plnk")
		})
	}
}

func TestRebuildIsIdempotent(t *testing.T) {
	src := buildSourceImage(t, testConfig, fixtureLayers(t, false)...)

	for _, mode := range []Mode{ModePreserveLayers, ModeSingleLayer} {
		t.Run(string(mode), func(t *testing.T) {
			first, _ := rebuildToImage(t, src, Options{Mode: mode})
			second, _ := rebuildToImage(t, first, Options{Mode: mode})

			firstDigest, err := first.Digest()
			require.NoError(t, err)
			secondDigest, err := second.Digest()
			require.NoError(t, err)
			assert.Equal(t, firstDigest.String(), secondDigest.String(),
				"rebuilding an already rebuilt image must not change it")

			// And rebuilding the image twice from the original source must
			// produce byte-identical manifests, not just equal layers.
			again, _ := rebuildToImage(t, src, Options{Mode: mode})
			againManifest, err := again.RawManifest()
			require.NoError(t, err)
			firstManifest, err := first.RawManifest()
			require.NoError(t, err)
			assert.Equal(t, string(firstManifest), string(againManifest))
		})
	}
}

func TestRebuildResultCloseRemovesScratchFiles(t *testing.T) {
	src := buildSourceImage(t, testConfig, fixtureLayers(t, false)...)

	root := t.TempDir()
	res, err := Rebuild(src, Options{ScratchDir: root})
	require.NoError(t, err)

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.NotEmpty(t, entries, "the rebuild must have created a scratch directory")

	require.NoError(t, res.Close())

	entries, err = os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries, "Close must remove the scratch directory")
}

func TestRebuildPreservesXattrs(t *testing.T) {
	entry := fileEntry("etc/xattr.txt", "xattr\n")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name:     entry.name,
		Typeflag: tar.TypeReg,
		Mode:     entry.mode,
		Uid:      entry.uid,
		Gid:      entry.gid,
		ModTime:  entry.modTime,
		Size:     int64(len(entry.body)),
		Format:   tar.FormatPAX,
		Xattrs:   map[string]string{"user.test": "value"},
	}))
	_, err := tw.Write([]byte(entry.body))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	layer, err := tarball.LayerFromReader(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)

	src := buildSourceImage(t, testConfig, layer)

	out, _ := rebuildToImage(t, src, Options{})

	found := false
	layers, err := out.Layers()
	require.NoError(t, err)
	for _, l := range layers {
		rc, err := l.Uncompressed()
		require.NoError(t, err)
		tr := tar.NewReader(rc)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			if hdr.Name == "etc/xattr.txt" {
				found = true
				assert.Equal(t, "value", hdr.Xattrs["user.test"])
			}
		}
		require.NoError(t, rc.Close())
	}
	assert.True(t, found, "file with xattrs must be present in the rebuilt image")
}

func TestRebuildDoesNotResurrectDeletedFiles(t *testing.T) {
	// The overriding file exists in three layers; the top version hides the
	// lower ones. Only the top version may survive.
	src := buildSourceImage(t, testConfig,
		layerFromEntries(t, []testEntry{fileEntry("etc/conf", "v1")}),
		layerFromEntries(t, []testEntry{fileEntry("etc/conf", "v2")}),
		layerFromEntries(t, []testEntry{fileEntry("etc/conf", "v3")}),
	)

	out, _ := rebuildToImage(t, src, Options{})

	want := finalViewHeaders(t, src)
	got := finalViewHeaders(t, out)
	assertHeadersEqual(t, want, got)
	assert.Equal(t, hexOf("v3"), got["/etc/conf"].sum)

	layers, err := out.Layers()
	require.NoError(t, err)
	occurrences := 0
	for _, layer := range layers {
		rc, err := layer.Uncompressed()
		require.NoError(t, err)
		tr := tar.NewReader(rc)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			if normalizePath(hdr.Name) == "/etc/conf" {
				occurrences++
			}
		}
		require.NoError(t, rc.Close())
	}
	assert.Equal(t, 1, occurrences, "the shadowed versions must not be written back")
}

//////////////////////////////////////////////////////////////
// config round trip

type configVariant struct {
	name string
	cfg  v1.Config
}

func configVariants() []configVariant {
	entrypoints := [][]string{
		{"/bin/app", "--serve"},
		{"/bin/sh", "-c", "/bin/app --serve"},
		nil,
	}
	cmds := [][]string{
		{"--port", "8080"},
		{"/bin/sh", "-c", "exec /bin/app"},
		nil,
	}
	healthchecks := []*v1.HealthConfig{
		nil,
		{
			Test:     []string{"CMD", "curl", "-f", "http://localhost:8080/"},
			Interval: 30 * time.Second,
			Retries:  3,
		},
		{
			Test:        []string{"CMD-SHELL", "curl -f http://localhost:8080/ || exit 1"},
			Timeout:     5 * time.Second,
			StartPeriod: 10 * time.Second,
		},
	}

	envs := [][]string{
		nil,
		{"PATH=/usr/bin:/bin"},
		// duplicate keys and unusual order must survive verbatim
		{"Z=last", "A=first", "PATH=/usr/bin:/bin", "Z=overridden"},
	}

	var variants []configVariant
	for ei, ep := range entrypoints {
		for ci, cmd := range cmds {
			for hi, hc := range healthchecks {
				for vi, env := range envs {
					cfg := v1.Config{
						User:         "1001:1001",
						ExposedPorts: map[string]struct{}{"8080/tcp": {}, "9090/udp": {}},
						Env:          env,
						Entrypoint:   ep,
						Cmd:          cmd,
						Volumes:      map[string]struct{}{"/data": {}, "/logs": {}},
						WorkingDir:   "/srv/app",
						Labels:       map[string]string{"maintainer": "test", "version": "1.0"},
						StopSignal:   "SIGTERM",
						ArgsEscaped:  ei == 1,
						Shell:        []string{"/bin/sh", "-c"},
						Healthcheck:  hc,
					}
					variants = append(variants, configVariant{
						name: fmt.Sprintf("entrypoint%d/cmd%d/healthcheck%d/env%d", ei, ci, hi, vi),
						cfg:  cfg,
					})
				}
			}
		}
	}
	return variants
}

func TestConfigRoundTrip(t *testing.T) {
	layers := fixtureLayers(t, false)

	for _, variant := range configVariants() {
		variant := variant
		for _, mode := range []Mode{ModePreserveLayers, ModeSingleLayer} {
			mode := mode
			t.Run(variant.name+"/"+string(mode), func(t *testing.T) {
				src := buildSourceImage(t, variant.cfg, layers...)
				out, _ := rebuildToImage(t, src, Options{Mode: mode})

				// Field by field.
				srcCfg, err := src.ConfigFile()
				require.NoError(t, err)
				outCfg, err := out.ConfigFile()
				require.NoError(t, err)

				assert.Equal(t, srcCfg.Config.User, outCfg.Config.User)
				assert.Equal(t, srcCfg.Config.WorkingDir, outCfg.Config.WorkingDir)
				assert.Equal(t, srcCfg.Config.StopSignal, outCfg.Config.StopSignal)
				assert.Equal(t, srcCfg.Config.ExposedPorts, outCfg.Config.ExposedPorts)
				assert.Equal(t, srcCfg.Config.Volumes, outCfg.Config.Volumes)
				assert.Equal(t, srcCfg.Config.Labels, outCfg.Config.Labels)
				assert.Equal(t, srcCfg.Config.Shell, outCfg.Config.Shell)
				assert.Equal(t, srcCfg.Config.ArgsEscaped, outCfg.Config.ArgsEscaped)

				// Exec form vs shell form must not be normalized into each other.
				assert.Equal(t, len(srcCfg.Config.Entrypoint), len(outCfg.Config.Entrypoint))
				assert.Equal(t, srcCfg.Config.Entrypoint, outCfg.Config.Entrypoint)
				assert.Equal(t, len(srcCfg.Config.Cmd), len(outCfg.Config.Cmd))
				assert.Equal(t, srcCfg.Config.Cmd, outCfg.Config.Cmd)

				// Env order and duplicate definitions are preserved.
				assert.Equal(t, srcCfg.Config.Env, outCfg.Config.Env)

				// Health check exec/shell form is preserved.
				assert.True(t, reflect.DeepEqual(srcCfg.Config.Healthcheck, outCfg.Config.Healthcheck),
					"healthcheck mismatch: want %+v got %+v", srcCfg.Config.Healthcheck, outCfg.Config.Healthcheck)

				// Platform and image metadata.
				assert.Equal(t, srcCfg.Architecture, outCfg.Architecture)
				assert.Equal(t, srcCfg.OS, outCfg.OS)
				assert.Equal(t, srcCfg.OSVersion, outCfg.OSVersion)
				assert.Equal(t, srcCfg.OSFeatures, outCfg.OSFeatures)
				assert.Equal(t, srcCfg.Variant, outCfg.Variant)
				assert.Equal(t, srcCfg.Author, outCfg.Author)
				assert.Equal(t, srcCfg.Container, outCfg.Container)
				assert.Equal(t, srcCfg.DockerVersion, outCfg.DockerVersion)
				assert.Equal(t, srcCfg.Created.Time.UTC(), outCfg.Created.Time.UTC())

				// The whole preserved section must serialize to identical bytes.
				assert.Equal(t, string(preservedConfigBytes(t, src)), string(preservedConfigBytes(t, out)),
					"config section must be byte-for-byte identical")
			})
		}
	}
}

func TestConfigRoundTripThroughGzipLayer(t *testing.T) {
	// Sanity check that a gzip-compressed layer input is accepted as well.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	raw := tarBytes(t, []testEntry{fileEntry("etc/x.txt", "x\n")})
	_, err := gw.Write(raw)
	require.NoError(t, err)
	require.NoError(t, gw.Close())

	layer, err := tarball.LayerFromReader(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)

	src := buildSourceImage(t, testConfig, layer)
	out, _ := rebuildToImage(t, src, Options{})
	want, _ := extractView(t, src)
	got, _ := extractView(t, out)
	assertViewsEqual(t, want, got)
}

func hexOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
