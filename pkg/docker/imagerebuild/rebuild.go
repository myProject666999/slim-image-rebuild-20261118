// Package imagerebuild rewrites a container image so that it only contains the
// files that are visible in the image's final filesystem view.
//
// The package is meant to be used by slimming pipelines that need to drop
// files from an image without changing what the image looks like at run time.
// Two layer organization strategies are supported:
//
//   - ModePreserveLayers (default): the original layer sequence is kept. Each
//     layer is rewritten to contain only the files whose version is the one
//     that wins in the final filesystem view. Layers that end up with no files
//     are dropped. Because layer boundaries are retained, images built on top
//     of the slimmed image can keep sharing the untouched lower layers, so
//     registries and local stores do not have to keep a second full copy of
//     the base image.
//
//   - ModeSingleLayer: the final filesystem view is packed into a single
//     layer. This produces the smallest output for a standalone image, but it
//     destroys layer reuse: every slimmed variant of the same base image gets
//     its own full copy of the base data, which can make the aggregate storage
//     footprint larger.
//
// In both modes the rewrite is a pure function of the source image:
//
//   - file order inside every produced layer is deterministic (the original
//     order is preserved in ModePreserveLayers; paths are sorted, with hard
//     link targets emitted before their link entries, in ModeSingleLayer),
//   - timestamps are copied from the source, never from the wall clock,
//   - the gzip parameters are fixed (see compressionLevel),
//   - the image history either is dropped or is copied from the source (see
//     Options.KeepHistory).
//
// As a result, rebuilding the same source image twice produces the same
// manifest digest.
package imagerebuild

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Mode selects how the rebuilt image organizes its layers.
type Mode string

const (
	// ModePreserveLayers keeps the original layer sequence (default).
	ModePreserveLayers Mode = "preserve-layers"
	// ModeSingleLayer packs the final filesystem view into one layer.
	ModeSingleLayer Mode = "single-layer"
)

// compressionLevel is the gzip level used for every produced layer. It is a
// constant (and not configurable) because changing it changes the layer
// digests, and stable digests are part of the package contract.
const compressionLevel = gzip.BestSpeed

const (
	whiteoutPrefix = ".wh."
	// whiteoutMetaPrefix marks the aufs metadata namespace (hard link store).
	whiteoutMetaPrefix = ".wh..wh."
	// whiteoutOpaqueDir makes the directory that contains it opaque.
	// Docker and containerd write ".wh..wh..opq"; a few producers use the
	// shorter ".wh..opq". Both are accepted on input, neither is written out.
	whiteoutOpaqueDir    = ".wh..wh..opq"
	whiteoutOpaqueDirAlt = ".wh..opq"
	// whiteoutLinkDir holds the aufs hard link targets.
	whiteoutLinkDir = ".wh..wh.plnk"
)

// Options controls the rebuild.
type Options struct {
	// Mode selects the layer organization strategy.
	// An empty value means ModePreserveLayers.
	Mode Mode

	// KeepHistory preserves the source image history.
	//
	// By default the history is dropped. Source history entries carry
	// timestamps and commands of builds that no longer describe the rewritten
	// layers, and image history often contains non-reproducible values (build
	// host names, build ids, timestamps). Dropping it keeps the output config a
	// pure function of the file payload, so the same filesystem view produced
	// from images with different histories still gets the same digest.
	//
	// When KeepHistory is set, ModePreserveLayers keeps the history entries
	// that belong to retained layers (entries with EmptyLayer == true are kept
	// unconditionally). ModeSingleLayer cannot keep per-layer entries because
	// layers are collapsed, so only empty-layer entries are retained there.
	KeepHistory bool

	// ScratchDir is an optional parent directory for the temporary files used
	// to spool layers. The system temp directory is used when empty.
	ScratchDir string
}

// Result is the outcome of a rebuild. Image is backed by temporary files, so
// Close must be called once the image has been consumed (written out, pushed,
// inspected, ...).
type Result struct {
	Image v1.Image

	Mode           Mode
	SourceLayers   int
	RetainedLayers int
	DroppedLayers  int
	FilesRetained  int
	// ReusedLayers counts the ModePreserveLayers layers that were copied
	// byte-for-byte from the source, keeping their layer digests.
	ReusedLayers int
	HistoryKept  bool

	scratchDir string
}

// Close removes the temporary files backing the result image.
func (r *Result) Close() error {
	if r == nil || r.scratchDir == "" {
		return nil
	}
	err := os.RemoveAll(r.scratchDir)
	r.scratchDir = ""
	return err
}

type entryKind int

const (
	kindDir entryKind = iota
	kindFile
	kindSymlink
	kindHardlink
	// kindWhiteout removes a path from lower layers.
	kindWhiteout
	// kindOpaque removes the lower-layer children of a directory.
	kindOpaque
	// kindMeta is aufs metadata (the hard link store); never part of the view.
	kindMeta
	// kindOther is a device, fifo, etc. preserved by header only.
	kindOther
)

type entry struct {
	layerIdx int
	idx      int

	header *tar.Header

	// name is the normalized absolute path of the entry.
	name string
	kind entryKind

	// whiteoutTarget is the normalized path removed by a whiteout entry.
	whiteoutTarget string
	// opaqueDir is the normalized directory made opaque by an opq entry.
	opaqueDir string
	// linkTargetPath is the normalized link target of a hard link.
	linkTargetPath string
	// linkResolved is the entry that holds the data a hard link refers to,
	// resolved in the filesystem snapshot at the layer that introduces the
	// link (nil when the link is broken or points into the aufs store).
	linkResolved *entry
	// linkAnchor is the surviving name a hard link is written against (only
	// set when the link is written as a tar link).
	linkAnchor string
	// linkOwnsData marks a materialized link, i.e. a retained name that writes
	// the group's bytes as a regular file.
	linkOwnsData bool
	// keepAsLink is set when the link can be written as a tar link.
	keepAsLink bool

	dataOffset int64
	hasData    bool
}

type layerSpool struct {
	file    *os.File
	entries []*entry
	// aufsLinks maps the aufs hard link store file names to their entries.
	aufsLinks map[string]*entry
}

// Rebuild rewrites src so that it only contains the final filesystem view.
func Rebuild(src v1.Image, opts Options) (*Result, error) {
	if src == nil {
		return nil, errors.New("imagerebuild: nil source image")
	}

	mode := opts.Mode
	if mode == "" {
		mode = ModePreserveLayers
	}
	if mode != ModePreserveLayers && mode != ModeSingleLayer {
		return nil, fmt.Errorf("imagerebuild: unsupported mode %q", mode)
	}

	scratchDir, err := os.MkdirTemp(opts.ScratchDir, "slim-image-rebuild-")
	if err != nil {
		return nil, fmt.Errorf("imagerebuild: create scratch dir: %w", err)
	}

	result := &Result{Mode: mode, scratchDir: scratchDir}
	success := false
	defer func() {
		if !success {
			_ = result.Close()
		}
	}()

	layers, err := src.Layers()
	if err != nil {
		return nil, fmt.Errorf("imagerebuild: read source layers: %w", err)
	}
	result.SourceLayers = len(layers)

	spools := make([]*layerSpool, 0, len(layers))
	for idx, layer := range layers {
		spool, err := spoolLayer(scratchDir, idx, layer)
		if err != nil {
			return nil, err
		}
		spools = append(spools, spool)
	}

	view := buildFinalView(spools)
	retained := make(map[*entry]bool, len(view))
	for _, e := range view {
		retained[e] = true
	}
	result.FilesRetained = len(view)

	planHardLinks(view, retained)

	layerMediaType, err := chooseLayerMediaType(src)
	if err != nil {
		return nil, err
	}

	var produced []v1.Layer
	keptLayerIdx := make([]int, 0, len(spools))

	addLayer := func(filePath string) error {
		layer, err := tarball.LayerFromFile(filePath,
			tarball.WithCompressionLevel(compressionLevel),
			tarball.WithMediaType(layerMediaType))
		if err != nil {
			return fmt.Errorf("imagerebuild: build layer from %s: %w", filePath, err)
		}
		produced = append(produced, layer)
		return nil
	}

	switch mode {
	case ModePreserveLayers:
		for idx, spool := range spools {
			// A layer whose entries are all retained verbatim and that carries
			// no whiteouts, opaque markers or aufs metadata can be copied as
			// is. Keeping the original blob preserves the layer digest, so the
			// slimmed image keeps sharing that layer with its base image and
			// with other images built on it.
			if reusableLayer(spool, retained) {
				produced = append(produced, layers[idx])
				keptLayerIdx = append(keptLayerIdx, idx)
				result.ReusedLayers++
				continue
			}

			outPath, count, err := writePreservedLayer(scratchDir, idx, spool, spools, retained)
			if err != nil {
				return nil, err
			}
			if count == 0 {
				result.DroppedLayers++
				continue
			}
			if err := addLayer(outPath); err != nil {
				return nil, err
			}
			keptLayerIdx = append(keptLayerIdx, idx)
		}
	case ModeSingleLayer:
		outPath, count, err := writeSingleLayer(scratchDir, view, spools)
		if err != nil {
			return nil, err
		}
		if count > 0 {
			if err := addLayer(outPath); err != nil {
				return nil, err
			}
			keptLayerIdx = append(keptLayerIdx, 0)
		}
	}

	result.RetainedLayers = len(produced)

	cfg, err := src.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("imagerebuild: read source config: %w", err)
	}
	outCfg := cfg.DeepCopy()

	diffIDs := make([]v1.Hash, 0, len(produced))
	for _, layer := range produced {
		diffID, err := layer.DiffID()
		if err != nil {
			return nil, fmt.Errorf("imagerebuild: compute layer diff id: %w", err)
		}
		diffIDs = append(diffIDs, diffID)
	}
	if outCfg.RootFS.Type == "" {
		outCfg.RootFS.Type = "layers"
	}
	outCfg.RootFS.DiffIDs = diffIDs
	outCfg.History = rebuildHistory(cfg.History, mode, keptLayerIdx, opts.KeepHistory)
	result.HistoryKept = opts.KeepHistory && len(outCfg.History) > 0

	appended, err := mutate.AppendLayers(empty.Image, produced...)
	if err != nil {
		return nil, fmt.Errorf("imagerebuild: append layers: %w", err)
	}

	img, err := mutate.ConfigFile(appended, outCfg)
	if err != nil {
		return nil, fmt.Errorf("imagerebuild: set config: %w", err)
	}

	srcMediaType, err := src.MediaType()
	if err != nil {
		return nil, fmt.Errorf("imagerebuild: read source media type: %w", err)
	}
	img = mutate.MediaType(img, srcMediaType)

	srcManifest, err := src.Manifest()
	if err != nil {
		return nil, fmt.Errorf("imagerebuild: read source manifest: %w", err)
	}
	if srcManifest.Config.MediaType != "" {
		img = mutate.ConfigMediaType(img, srcManifest.Config.MediaType)
	}
	if len(srcManifest.Annotations) > 0 {
		annotations := make(map[string]string, len(srcManifest.Annotations))
		for k, v := range srcManifest.Annotations {
			annotations[k] = v
		}
		if annotated, ok := mutate.Annotations(img, annotations).(v1.Image); ok {
			img = annotated
		}
	}

	result.Image = img
	success = true
	return result, nil
}

func chooseLayerMediaType(src v1.Image) (types.MediaType, error) {
	mt, err := src.MediaType()
	if err != nil {
		return "", fmt.Errorf("imagerebuild: read source media type: %w", err)
	}
	switch mt {
	case types.OCIManifestSchema1:
		return types.OCILayer, nil
	default:
		return types.DockerLayer, nil
	}
}

func rebuildHistory(src []v1.History, mode Mode, keptLayerIdx []int, keep bool) []v1.History {
	if !keep {
		return nil
	}

	kept := make(map[int]bool, len(keptLayerIdx))
	for _, idx := range keptLayerIdx {
		kept[idx] = true
	}

	var out []v1.History
	layerIdx := 0
	for _, h := range src {
		if h.EmptyLayer {
			out = append(out, *h.DeepCopy())
			continue
		}
		if mode == ModePreserveLayers && kept[layerIdx] {
			out = append(out, *h.DeepCopy())
		}
		layerIdx++
	}
	return out
}

// planHardLinks decides how every retained hard link is written out:
//
//   - if the entry that holds the data is itself the winning entry of its path
//     in the final view, every link to it stays a tar link to that name, so no
//     data is duplicated and the inode relationship is kept;
//   - otherwise the data entry is gone from the rewrite (for instance because
//     a later layer replaced that path). All retained names that still refer
//     to the same data form a group; the earliest name in the group writes the
//     bytes as a regular file and the rest link to it. This keeps the names
//     hard linked to each other, and when the group shrank to a single name
//     that name simply holds the content;
//   - a broken link (no resolvable data) is copied unchanged.
func planHardLinks(view map[string]*entry, retained map[*entry]bool) {
	type group struct {
		members []*entry
	}

	var groups []*group
	groupByData := map[*entry]*group{}
	resolve := func(link *entry) *entry {
		seen := map[*entry]bool{}
		e := link
		for e != nil && e.kind == kindHardlink {
			if seen[e] {
				return nil
			}
			seen[e] = true
			e = e.linkResolved
		}
		return e
	}

	for e := range retained {
		if e.kind != kindHardlink {
			continue
		}

		data := resolve(e)
		if data != nil && retained[data] && view[data.name] == data &&
			(data.kind == kindFile || data.kind == kindOther || data.kind == kindSymlink) {
			// The data entry survived as the winner of its path.
			e.keepAsLink = true
			e.linkAnchor = data.header.Name
			continue
		}

		if data == nil || (data.kind != kindFile && data.kind != kindOther && data.kind != kindMeta) {
			// Broken link or non-materializable target: keep the source header.
			continue
		}

		g := groupByData[data]
		if g == nil {
			g = &group{}
			groupByData[data] = g
			groups = append(groups, g)
		}
		g.members = append(g.members, e)
	}

	for _, g := range groups {
		sort.Slice(g.members, func(i, j int) bool {
			// Pick the member that is written first in both modes: layers are
			// emitted in order, and inside a layer the original entry order is
			// kept, so the anchor is always present before the links that
			// point at it.
			a, b := g.members[i], g.members[j]
			if a.layerIdx != b.layerIdx {
				return a.layerIdx < b.layerIdx
			}
			if a.idx != b.idx {
				return a.idx < b.idx
			}
			return a.name < b.name
		})

		anchor := g.members[0]
		anchor.linkOwnsData = true
		anchor.keepAsLink = false

		for _, e := range g.members[1:] {
			e.keepAsLink = true
			e.linkAnchor = anchor.header.Name
		}
	}

	// Point links at their original target when that name is retained and is
	// itself written before the link, so chains like a <- b <- c survive
	// instead of being flattened onto the group anchor.
	byName := make(map[string]*entry, len(view))
	for _, e := range view {
		byName[e.name] = e
	}
	for e := range retained {
		if e.kind != kindHardlink || !e.keepAsLink {
			continue
		}
		target := byName[e.linkTargetPath]
		if target == nil || !retained[target] {
			continue
		}
		if !target.keepAsLink && !target.linkOwnsData {
			continue
		}
		if target.layerIdx > e.layerIdx ||
			(target.layerIdx == e.layerIdx && target.idx >= e.idx) {
			continue
		}
		e.linkAnchor = target.header.Name
	}
}

// buildFinalView applies the layers in order and returns the entries that are
// visible in the final filesystem view (one winner per path).
func buildFinalView(spools []*layerSpool) map[string]*entry {
	state := map[string]*entry{}

	for _, spool := range spools {
		for _, e := range spool.entries {
			switch e.kind {
			case kindMeta:
				// Aufs metadata is never visible.
			case kindOpaque:
				// Only hides the entries that come from lower layers; entries
				// unpacked from this layer (before or after the marker) stay.
				deleteLowerLayerChildren(state, e.opaqueDir, e.layerIdx)
			case kindWhiteout:
				deleteSubtree(state, e.whiteoutTarget)
			case kindDir:
				if existing, ok := state[e.name]; ok && existing.kind != kindDir {
					deleteSubtree(state, e.name)
				}
				state[e.name] = e
			case kindHardlink:
				e.linkResolved = resolveLinkTarget(state, spool, e)
				deleteSubtree(state, e.name)
				state[e.name] = e
			default:
				deleteSubtree(state, e.name)
				state[e.name] = e
			}
		}
	}

	return state
}

func resolveLinkTarget(state map[string]*entry, spool *layerSpool, e *entry) *entry {
	target := e.linkTargetPath
	if strings.HasPrefix(target, "/"+whiteoutLinkDir+"/") {
		if aufs, ok := spool.aufsLinks[path.Base(target)]; ok {
			return aufs
		}
		return nil
	}

	seen := map[*entry]bool{}
	found := state[target]
	for found != nil && found.kind == kindHardlink && !seen[found] {
		seen[found] = true
		if found.linkResolved == nil {
			return nil
		}
		found = found.linkResolved
	}
	return found
}

func deleteSubtree(state map[string]*entry, target string) {
	delete(state, target)
	prefix := target
	if prefix != "/" {
		prefix += "/"
	}
	for p := range state {
		if strings.HasPrefix(p, prefix) {
			delete(state, p)
		}
	}
}

func deleteLowerLayerChildren(state map[string]*entry, dir string, layerIdx int) {
	prefix := dir
	if prefix != "/" {
		prefix += "/"
	}
	for p, e := range state {
		if p == dir || e.layerIdx >= layerIdx {
			continue
		}
		if strings.HasPrefix(p, prefix) {
			delete(state, p)
		}
	}
}

func spoolLayer(scratchDir string, idx int, layer v1.Layer) (*layerSpool, error) {
	rc, err := layer.Uncompressed()
	if err != nil {
		return nil, fmt.Errorf("imagerebuild: open layer %d: %w", idx, err)
	}
	defer rc.Close()

	f, err := os.CreateTemp(scratchDir, fmt.Sprintf("layer-%03d-*.tar", idx))
	if err != nil {
		return nil, fmt.Errorf("imagerebuild: spool layer %d: %w", idx, err)
	}

	spool := &layerSpool{file: f}
	ok := false
	defer func() {
		if !ok {
			spool.Close()
		}
	}()

	if _, err := io.Copy(f, rc); err != nil {
		return nil, fmt.Errorf("imagerebuild: spool layer %d: %w", idx, err)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("imagerebuild: rewind layer %d: %w", idx, err)
	}

	entries, aufsLinks, err := indexLayer(f, idx)
	if err != nil {
		return nil, err
	}
	spool.entries = entries
	spool.aufsLinks = aufsLinks
	ok = true
	return spool, nil
}

func (l *layerSpool) Close() {
	if l == nil || l.file == nil {
		return
	}
	name := l.file.Name()
	_ = l.file.Close()
	_ = os.Remove(name)
	l.file = nil
}

// offsetReader reports the byte offset of the underlying reader, so the tar
// entry data offsets can be recorded without buffering the layer in memory.
type offsetReader struct {
	r   io.Reader
	off int64
}

func (o *offsetReader) Read(p []byte) (int, error) {
	n, err := o.r.Read(p)
	o.off += int64(n)
	return n, err
}

func indexLayer(f *os.File, layerIdx int) ([]*entry, map[string]*entry, error) {
	or := &offsetReader{r: f}
	tr := tar.NewReader(or)
	var entries []*entry
	aufsLinks := map[string]*entry{}

	for {
		// The tar reader consumes padding and any PAX/GNU meta headers as part
		// of Next, so the underlying offset is exactly where the entry data
		// starts.
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("imagerebuild: read layer %d tar: %w", layerIdx, err)
		}
		if hdr == nil || hdr.Name == "" {
			continue
		}

		e := classifyEntry(hdr, layerIdx, len(entries))

		if e.kind == kindFile || e.kind == kindOther || e.kind == kindMeta {
			e.dataOffset = or.off
			e.hasData = hdr.Size > 0
			if _, err := io.CopyN(io.Discard, tr, hdr.Size); err != nil && !errors.Is(err, io.EOF) {
				return nil, nil, fmt.Errorf("imagerebuild: skip layer %d entry %q: %w", layerIdx, hdr.Name, err)
			}
		}

		entries = append(entries, e)
		if e.kind == kindMeta && (e.header.Typeflag == tar.TypeReg || e.header.Typeflag == tar.TypeRegA) {
			aufsLinks[path.Base(e.name)] = e
		}
	}

	return entries, aufsLinks, nil
}

func classifyEntry(hdr *tar.Header, layerIdx, idx int) *entry {
	e := &entry{
		layerIdx: layerIdx,
		idx:      idx,
		header:   hdr,
		name:     normalizePath(hdr.Name),
	}

	base := path.Base(e.name)
	dir := path.Dir(e.name)

	switch {
	case base == whiteoutOpaqueDir || base == whiteoutOpaqueDirAlt:
		e.kind = kindOpaque
		e.opaqueDir = dir
	case strings.HasPrefix(base, whiteoutMetaPrefix):
		// Aufs metadata (notably the hard link store). Dropped from the view;
		// the links that reference it are retargeted in resolveLinkTarget.
		e.kind = kindMeta
	case strings.HasPrefix(base, whiteoutPrefix):
		e.kind = kindWhiteout
		e.whiteoutTarget = path.Join(dir, strings.TrimPrefix(base, whiteoutPrefix))
	case hdr.Typeflag == tar.TypeDir:
		e.kind = kindDir
	case hdr.Typeflag == tar.TypeLink:
		e.kind = kindHardlink
		e.linkTargetPath = normalizePath(hdr.Linkname)
	case hdr.Typeflag == tar.TypeSymlink:
		e.kind = kindSymlink
	case hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA:
		e.kind = kindFile
	default:
		// Devices, fifos, etc. are preserved by header only.
		e.kind = kindOther
	}

	return e
}

func normalizePath(name string) string {
	p := path.Clean("/" + strings.TrimPrefix(name, "/"))
	if p == "" {
		return "/"
	}
	return p
}

// reusableLayer reports whether a source layer can be copied verbatim: every
// entry is retained and written with its original header, and the layer
// contains no whiteout, opaque or aufs metadata entries that must disappear
// from the output.
func reusableLayer(spool *layerSpool, retained map[*entry]bool) bool {
	if len(spool.entries) == 0 {
		return false
	}
	for _, e := range spool.entries {
		if !retained[e] {
			return false
		}
		switch e.kind {
		case kindHardlink:
			// A link keeps its blob only when it is still written against the
			// very same name it pointed at in the source.
			if e.linkOwnsData {
				return false
			}
			if e.keepAsLink && normalizePath(e.linkAnchor) != e.linkTargetPath {
				return false
			}
		case kindWhiteout, kindOpaque, kindMeta:
			return false
		}
	}
	return true
}

func writePreservedLayer(
	scratchDir string,
	idx int,
	spool *layerSpool,
	spools []*layerSpool,
	retained map[*entry]bool,
) (string, int, error) {

	outPath := filepath.Join(scratchDir, fmt.Sprintf("out-layer-%03d.tar", idx))

	count, err := writeTar(outPath, func(tw *tar.Writer) (int, error) {
		written := 0
		// Original order is preserved: it is deterministic and keeps hard
		// link anchors ahead of the links that point at them.
		for _, e := range spool.entries {
			if !retained[e] {
				continue
			}
			if err := writeEntry(tw, spools, e); err != nil {
				return written, err
			}
			written++
		}
		return written, nil
	})
	if err != nil {
		return "", 0, err
	}
	return outPath, count, nil
}

func writeSingleLayer(
	scratchDir string,
	view map[string]*entry,
	spools []*layerSpool,
) (string, int, error) {

	// Deterministic order: everything that is not a tar link is sorted by
	// path (parents sort before their children), then the links are written
	// so that the name they point at is already present.
	var plain []*entry
	var links []*entry
	for _, e := range view {
		if e.kind == kindHardlink && e.keepAsLink {
			links = append(links, e)
			continue
		}
		plain = append(plain, e)
	}
	sort.Slice(plain, func(i, j int) bool { return plain[i].name < plain[j].name })
	sort.Slice(links, func(i, j int) bool { return links[i].name < links[j].name })

	outPath := filepath.Join(scratchDir, "out-layer-single.tar")
	count, err := writeTar(outPath, func(tw *tar.Writer) (int, error) {
		written := 0
		for _, e := range plain {
			if err := writeEntry(tw, spools, e); err != nil {
				return written, err
			}
			written++
		}

		// Hard link chains can be arbitrarily deep; write every link after the
		// name it points at, in name order for determinism.
		emitted := make(map[string]bool, len(links))
		byName := make(map[string]*entry, len(links))
		for _, e := range links {
			byName[e.name] = e
		}
		var emit func(e *entry) error
		emit = func(e *entry) error {
			if emitted[e.name] {
				return nil
			}
			emitted[e.name] = true
			if target := byName[normalizePath(e.linkAnchor)]; target != nil {
				if err := emit(target); err != nil {
					return err
				}
			}
			written++
			return writeEntry(tw, spools, e)
		}
		for _, e := range links {
			if err := emit(e); err != nil {
				return written, err
			}
		}
		return written, nil
	})
	if err != nil {
		return "", 0, err
	}
	return outPath, count, nil
}

func writeTar(outPath string, fill func(tw *tar.Writer) (int, error)) (int, error) {
	f, err := os.Create(outPath)
	if err != nil {
		return 0, fmt.Errorf("imagerebuild: create %s: %w", outPath, err)
	}
	defer f.Close()

	bw := bufio.NewWriter(f)
	tw := tar.NewWriter(bw)
	count, err := fill(tw)
	if err != nil {
		return 0, err
	}
	if err := tw.Close(); err != nil {
		return 0, fmt.Errorf("imagerebuild: close tar %s: %w", outPath, err)
	}
	if err := bw.Flush(); err != nil {
		return 0, fmt.Errorf("imagerebuild: flush tar %s: %w", outPath, err)
	}
	return count, nil
}

func writeEntry(tw *tar.Writer, spools []*layerSpool, e *entry) error {
	if e.kind != kindHardlink {
		return writeHeaderAndData(tw, spools, e, cloneHeader(e.header))
	}

	if e.keepAsLink {
		hdr := cloneHeader(e.header)
		hdr.Linkname = e.linkAnchor
		return writeHeaderAndData(tw, spools, e, hdr)
	}

	if e.linkOwnsData {
		if hdr, source, ok := materializeLink(e); ok {
			return writeHeaderAndData(tw, spools, source, hdr)
		}
	}

	// Broken link in the source image: keep it as-is.
	return writeHeaderAndData(tw, spools, e, cloneHeader(e.header))
}

// materializeLink turns a hard link entry into a regular file header that
// reuses the data of the resolved target entry.
//
// The header metadata (mode, ownership, timestamps, extended attributes) is
// taken from the entry that originally created the inode rather than from the
// link header: both names describe the same inode, and the inode is what the
// final view exposes.
func materializeLink(e *entry) (*tar.Header, *entry, bool) {
	target := e.linkResolved
	if target == nil || (target.kind != kindFile && target.kind != kindOther && target.kind != kindMeta) {
		return nil, nil, false
	}

	hdr := cloneHeader(target.header)
	hdr.Name = e.header.Name
	hdr.Typeflag = tar.TypeReg
	hdr.Linkname = ""
	hdr.Size = target.header.Size
	hdr.Format = tar.FormatPAX
	return hdr, target, true
}

func writeHeaderAndData(tw *tar.Writer, spools []*layerSpool, e *entry, hdr *tar.Header) error {
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("imagerebuild: write header %q: %w", hdr.Name, err)
	}
	if hdr.Size <= 0 {
		return nil
	}

	if !e.hasData || e.dataOffset < 0 || e.layerIdx < 0 || e.layerIdx >= len(spools) {
		return fmt.Errorf("imagerebuild: entry %q has no data", hdr.Name)
	}

	src := io.NewSectionReader(spools[e.layerIdx].file, e.dataOffset, e.header.Size)
	if _, err := io.CopyN(tw, src, e.header.Size); err != nil {
		return fmt.Errorf("imagerebuild: copy data for %q: %w", hdr.Name, err)
	}
	return nil
}

func cloneHeader(hdr *tar.Header) *tar.Header {
	out := *hdr
	// PAX is requested so that long names, sub-second timestamps, access/change
	// times and extended attributes survive the rewrite. The tar writer still
	// falls back to USTAR when every field fits, and it sorts PAX records, so
	// the output bytes are a pure function of the header.
	out.Format = tar.FormatPAX
	if hdr.PAXRecords != nil {
		out.PAXRecords = make(map[string]string, len(hdr.PAXRecords))
		for k, v := range hdr.PAXRecords {
			out.PAXRecords[k] = v
		}
	}
	if hdr.Xattrs != nil {
		out.Xattrs = make(map[string]string, len(hdr.Xattrs))
		for k, v := range hdr.Xattrs {
			out.Xattrs[k] = v
		}
	}
	return &out
}
